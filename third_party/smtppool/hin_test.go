package smtppool

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// hinSMTP models a server that accepts consecutive authenticated messages,
// but rejects the next MAIL after RSET. It can also fail one RCPT transaction.
type hinSMTP struct {
	listener net.Listener
	mu       sync.Mutex
	clients  []net.Conn
	conns    int
	auths    int
	messages int
	resets   int
	failRCPT bool
	dropRCPT bool
	wg       sync.WaitGroup
}

func newHINSMTP(t *testing.T) *hinSMTP {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &hinSMTP{listener: l}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.clients = append(s.clients, c)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				s.serve(c)
			}()
		}
	}()
	return s
}

func (s *hinSMTP) close() {
	s.listener.Close()
	s.mu.Lock()
	for _, c := range s.clients {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *hinSMTP) serve(c net.Conn) {
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	reply := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format+"\r\n", args...); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	if !reply("220 mock.hin.test ESMTP") {
		return
	}
	authed, poisoned, completed := false, false, false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO "), strings.HasPrefix(cmd, "HELO "):
			if !reply("250-mock.hin.test\r\n250 AUTH LOGIN") {
				return
			}
		case cmd == "AUTH LOGIN":
			if !reply("334 VXNlcm5hbWU6") {
				return
			}
			user, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if !reply("334 UGFzc3dvcmQ6") {
				return
			}
			pass, err := r.ReadString('\n')
			if err != nil {
				return
			}
			u, uErr := base64.StdEncoding.DecodeString(strings.TrimSpace(user))
			p, pErr := base64.StdEncoding.DecodeString(strings.TrimSpace(pass))
			if uErr != nil || pErr != nil || string(u) != "user" || string(p) != "secret" {
				if !reply("535 Authentication failed") {
					return
				}
				continue
			}
			authed = true
			s.mu.Lock()
			s.auths++
			s.mu.Unlock()
			if !reply("235 Authentication succeeded") {
				return
			}
		case cmd == "RSET":
			if completed {
				poisoned = true
			}
			s.mu.Lock()
			s.resets++
			s.mu.Unlock()
			if !reply("250 Reset accepted") {
				return
			}
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			if !authed || poisoned {
				if !reply("550 Invalid request parameters") {
					return
				}
			} else if !reply("250 Sender accepted") {
				return
			}
		case strings.HasPrefix(cmd, "RCPT TO:"):
			s.mu.Lock()
			fail, drop := s.failRCPT, s.dropRCPT
			s.failRCPT, s.dropRCPT = false, false
			s.mu.Unlock()
			if drop {
				return
			}
			if fail {
				if !reply("550 Invalid request parameters") {
					return
				}
			} else if !reply("250 Recipient accepted") {
				return
			}
		case cmd == "DATA":
			if !reply("354 End data with <CRLF>.<CRLF>") {
				return
			}
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" {
					break
				}
			}
			completed = true
			s.mu.Lock()
			s.messages++
			s.mu.Unlock()
			if !reply("250 Message accepted") {
				return
			}
		case cmd == "QUIT":
			reply("221 Bye")
			return
		default:
			if !reply("500 Unknown command") {
				return
			}
		}
	}
}

func (s *hinSMTP) pool(t *testing.T, retries int) *Pool {
	t.Helper()
	addr := s.listener.Addr().(*net.TCPAddr)
	p, err := New(Opt{
		Host: "127.0.0.1", Port: addr.Port, MaxConns: 1,
		MaxMessageRetries: retries, PoolWaitTimeout: time.Second,
		Auth: &LoginAuth{Username: "user", Password: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hinEmail() Email {
	return Email{From: "sender@example.com", To: []string{"recipient@example.com"}, Text: []byte("hello")}
}

func TestHINMockRejectsSecondMessageAfterRSET(t *testing.T) {
	s := newHINSMTP(t)
	defer s.close()
	c, err := smtp.Dial(s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Auth(&LoginAuth{Username: "user", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("recipient@example.com"); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "Subject: first\r\n\r\nhello\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	var smtpErr *textproto.Error
	if err := c.Mail("sender@example.com"); !errors.As(err, &smtpErr) || smtpErr.Code != 550 {
		t.Fatalf("MAIL after RSET: expected SMTP 550, got %v", err)
	}
}

func TestHINReuseWithoutRSET(t *testing.T) {
	s := newHINSMTP(t)
	defer s.close()
	p := s.pool(t, 1)
	defer p.Close()
	for i := 0; i < 2; i++ {
		if err := p.Send(hinEmail()); err != nil {
			t.Fatalf("message %d: %v", i+1, err)
		}
	}
	s.mu.Lock()
	conns, auths, messages, resets := s.conns, s.auths, s.messages, s.resets
	s.mu.Unlock()
	if conns != 1 || auths != 1 || messages != 2 || resets != 0 {
		t.Fatalf("connections=%d auths=%d messages=%d RSETs=%d", conns, auths, messages, resets)
	}
	if got := p.createdConns.Load(); got != 1 {
		t.Fatalf("pool connection count=%d, want 1", got)
	}
}

func TestHINFailureDiscardsConnection(t *testing.T) {
	s := newHINSMTP(t)
	s.failRCPT = true
	defer s.close()
	p := s.pool(t, 2)
	defer p.Close()
	var smtpErr *textproto.Error
	if err := p.Send(hinEmail()); !errors.As(err, &smtpErr) || smtpErr.Code != 550 {
		t.Fatalf("failed RCPT: expected SMTP 550, got %v", err)
	}
	if got := p.createdConns.Load(); got != 0 {
		t.Fatalf("pool retained failed connection: count=%d", got)
	}
	if err := p.Send(hinEmail()); err != nil {
		t.Fatalf("send on new connection: %v", err)
	}
	s.mu.Lock()
	conns, auths, messages, resets := s.conns, s.auths, s.messages, s.resets
	s.mu.Unlock()
	if conns != 2 || auths != 2 || messages != 1 || resets != 0 {
		t.Fatalf("connections=%d auths=%d messages=%d RSETs=%d", conns, auths, messages, resets)
	}
}

func TestHINNetworkFailureRetainsRetrySemantics(t *testing.T) {
	s := newHINSMTP(t)
	s.dropRCPT = true
	defer s.close()
	p := s.pool(t, 2)
	defer p.Close()
	if err := p.Send(hinEmail()); err != nil {
		t.Fatalf("network failure should retry on a new connection: %v", err)
	}
	s.mu.Lock()
	conns, auths, messages := s.conns, s.auths, s.messages
	s.mu.Unlock()
	if conns != 2 || auths != 2 || messages != 1 {
		t.Fatalf("connections=%d auths=%d messages=%d", conns, auths, messages)
	}
}
