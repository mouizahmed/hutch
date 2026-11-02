package server

import (
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/api"
)

// A box that can't move stops machine rm: the machine stays, with its boxes, and
// takes new ones again.
func TestRmStopsAtABoxThatCantMove(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(left, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	defer right.Close()
	if err := s.store.ensureWorker("w-a", "a"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.workers["w-a"] = &workerConn{id: "w-a", sess: sess, info: api.Worker{Name: "a"}, projects: map[string]bool{}}
	s.mu.Unlock()
	if err := s.store.putBox(api.Box{ID: "tmpl", Worker: "w-a", State: "asleep", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.putMeta("tmpl", boxMeta{project: "p", kind: api.BoxTemplate}); err != nil {
		t.Fatal(err)
	}
	rm := func() error {
		p, _ := json.Marshal(api.IDParams{ID: "w-a"})
		_, err := s.workerRm(t.Context(), p, nil)
		return err
	}
	if err := rm(); err == nil || !strings.Contains(err.Error(), "template box") {
		t.Fatalf("rm with a template box on it: %v", err)
	}
	ws, err := s.store.workers()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Gone || ws[0].Draining {
		t.Fatalf("after a stopped drain: %+v", ws)
	}

	if err := s.store.deleteBox("tmpl", "test"); err != nil {
		t.Fatal(err)
	}
	if err := rm(); err != nil {
		t.Fatal(err)
	}
	if ws, _ = s.store.workers(); !ws[0].Gone {
		t.Fatal("an empty machine wasn't removed")
	}
}
