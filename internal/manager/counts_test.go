package manager

import (
	"io"
	"log"
	"testing"

	"github.com/knadh/listmonk/models"
)

type blockingCountStore struct {
	Store
	sent    chan int
	release chan struct{}
}

func (s *blockingCountStore) UpdateCampaignCounts(_ int, _, sent, _ int) error {
	s.sent <- sent
	<-s.release
	return nil
}

func TestCleanupDoesNotDoubleCountConcurrentCheckpoint(t *testing.T) {
	store := &blockingCountStore{
		sent:    make(chan int, 1),
		release: make(chan struct{}),
	}
	m := &Manager{
		store: store,
		log:   log.New(io.Discard, "", 0),
		pipes: make(map[int]*pipe),
	}
	p := &pipe{camp: &models.Campaign{Base: models.Base{ID: 1}, Name: "test"}, m: m}
	p.stopped.Store(true)
	p.sent.Store(4)
	m.pipes[1] = p

	done := make(chan struct{})
	go func() {
		p.cleanup()
		close(done)
	}()

	// Hold cleanup in the DB update while a periodic scan snapshots the pipe.
	// Only one of them may transfer the four successful sends.
	cleanupSent := <-store.sent
	ids, checkpointSent := m.getCurrentCampaigns()
	close(store.release)
	<-done

	if cleanupSent != 4 || len(ids) != 1 || ids[0] != 1 ||
		len(checkpointSent) != 1 || checkpointSent[0] != 0 {
		t.Fatalf("cleanup sent %d, checkpoint ids %v, sent %v; want 4 and 0",
			cleanupSent, ids, checkpointSent)
	}
}
