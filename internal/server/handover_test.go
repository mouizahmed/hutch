package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// A handover waits for changes under way and refuses new ones; one that
// doesn't finish in time calls it off, and changes go on.
func TestHandoverPausesChanges(t *testing.T) {
	s := &Server{}
	started, finish := make(chan struct{}), make(chan struct{})
	mux := rpc.Mux{
		api.ConfigPut: func(context.Context, json.RawMessage, *rpc.Out) (any, error) {
			close(started)
			<-finish
			return nil, nil
		},
		api.BoxList: func(context.Context, json.RawMessage, *rpc.Out) (any, error) { return nil, nil },
	}
	for _, m := range changing {
		if mux[m] == nil {
			mux[m] = func(context.Context, json.RawMessage, *rpc.Out) (any, error) { return nil, nil }
		}
	}
	s.gated(mux)
	call := func(m string) error {
		_, err := mux[m](context.Background(), nil, nil)
		return err
	}

	go call(api.ConfigPut)
	<-started
	if err := s.pause(50 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "handover_timeout") {
		t.Fatalf("a pause past a change under way: %v", err)
	}
	if err := call(api.BoxBoot); err != nil {
		t.Fatalf("a change after a called-off handover: %v", err)
	}

	paused := make(chan error)
	go func() { paused <- s.pause(5 * time.Second) }()
	time.Sleep(20 * time.Millisecond)
	if err := call(api.ProjectInit); err == nil || !strings.Contains(err.Error(), "handing the lead") {
		t.Fatalf("a new change during a handover: %v", err)
	}
	if err := call(api.BoxList); err != nil {
		t.Fatalf("a read during a handover: %v", err)
	}
	select {
	case err := <-paused:
		t.Fatalf("the pause didn't wait for the change under way: %v", err)
	default:
	}
	close(finish)
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
}

// The new lead's copy has the old lead as a linked worker, and the new lead's
// address and term.
func TestHandoverCopy(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ensureLink("old.example"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.ensureWorker("w-old", "old"); err != nil {
		t.Fatal(err)
	}
	data, err := s.handoverCopy(t.Context(), "w-old", "new.example", 5)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/copy.db"
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var linked, term int
	var address string
	if err := db.QueryRow(`SELECT linked FROM workers WHERE id = 'w-old'`).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("the old lead's row: linked %d %v", linked, err)
	}
	if err := db.QueryRow(`SELECT term, address FROM link WHERE id = 1`).Scan(&term, &address); err != nil || term != 5 || address != "new.example" {
		t.Fatalf("the link: term %d, address %q, %v", term, address, err)
	}
	if l, _ := s.store.link(); l.term != 1 || l.address != "old.example" {
		t.Fatalf("the old lead's own database changed: %+v", l)
	}
}

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// Lead state set aside keeps the link CA's certificate beside it, so the
// machine is known as one of this setup's when it's added back.
func TestSetAsideKeepsTheLinkCA(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.store.ensureLink("lead.example")
	if err != nil {
		t.Fatal(err)
	}
	s.store.db.Close()
	if err := SetAside(dir); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "stale-lead-*", "ca.pem"))
	if len(matches) != 1 {
		t.Fatalf("ca.pem beside the state: %v", matches)
	}
	got, _ := os.ReadFile(matches[0])
	if string(got) != string(l.ca.PEM) {
		t.Fatal("the kept CA isn't the link CA")
	}
	if _, err := os.Stat(filepath.Join(dir, "hutch.db")); err == nil {
		t.Fatal("the database wasn't set aside")
	}
}
