# HIN SMTP connection reuse patch

This branch starts at Listmonk `v6.2.0` (commit `ef0a75872463f10a4848af6c547d1c057405453a`). That release requires `github.com/knadh/smtppool/v2 v2.1.2`. `third_party/smtppool` is a copy of that module (Go module checksum `h1:FBpwnDTKmWwUy1reeeMeYJXl2QbUgVsKhAGVgJF3F2I=`, upstream tag commit `f2ab51026aa8c48a8daff28ff2e1649183164604`) with a change confined to `pool.go`. The root `go.mod` replaces the module with this local copy.

## Why

On `smtp.mail.hin.ch:587`, a persistent authenticated connection works for consecutive messages after a successful DATA completion. Sending `RSET` between those messages can make the next transaction fail with `550 Invalid request parameters`. In smtppool v2.1.2, `returnConn()` sent `RSET` before every reuse and kept connections after some SMTP errors.

The patched `returnConn()` pools a connection only when the previous transaction completed successfully. It sends no `RSET`. On any failed transaction, it closes the connection and decrements the pool's connection count. Existing queue waits, idle sweeping, timeout handling, and `Send()` retry decisions are unchanged. Network errors remain eligible for retry according to the existing `canRetry()` logic; an SMTP `550` remains non-retryable.

## Test

From the repository root, using Go 1.26.1 or newer:

```sh
go test ./internal/messenger/email
(cd third_party/smtppool && go test -count=1 pool.go email.go hin_test.go)
go list -m -f '{{.Path}} {{.Version}} => {{.Replace.Dir}}' github.com/knadh/smtppool/v2
```

The focused tests use a local mock SMTP server and do not contact HIN or send real mail. They prove that the mock accepts authentication, returns `550` on the next message after `RSET`, accepts two messages on one connection without `RSET`, discards a connection after SMTP failure, and keeps the existing network retry behavior. The copied upstream `pool_test.go` uses a MailHog `TestMain`, so the focused command names only the production files and `hin_test.go`.

## Linux Docker image

```sh
docker build --platform=linux/amd64 -f Dockerfile.hin -t listmonk:hin-v6.2.0 .
docker run --rm --entrypoint ./listmonk listmonk:hin-v6.2.0 --version
```

`Dockerfile.hin` pins its base image digests, builds both JavaScript frontends from their tracked Yarn lockfiles, includes Altcha and Listmonk's public/static assets, runs the focused SMTP tests, builds the Linux binary from the local replacement module, and packs the assets with the release's `stuffbin` dependency. The build checks the binary's Go module metadata for `third_party/smtppool` before producing the runtime image. The runtime stage keeps the release's entrypoint and configuration path. Building or checking the image does not contact the configured SMTP server.

To inspect the binary from the final image with a host Go installation:

```sh
id=$(docker create --entrypoint ./listmonk listmonk:hin-v6.2.0 --version)
docker cp "$id:/listmonk/listmonk" ./listmonk-hin
docker rm "$id"
go version -m ./listmonk-hin
```

The `go version -m` output should show `github.com/knadh/smtppool/v2 v2.1.2` followed by `=> ./third_party/smtppool`.

## Revert

For a deployed instance, switch back to the original Listmonk `v6.2.0` image or binary. To remove this source patch, revert the commit containing `go.mod`, `third_party/smtppool`, `Dockerfile.hin`, this document, and the cache exclusions, then rebuild. No database migration, credentials change, subscriber edit, or message resend is involved.
