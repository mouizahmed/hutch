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
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
)

// rm --lost is only for a machine that's offline, and plain rm only for one
// that's connected. A lost machine's boxes in the bucket stay, to come back
// elsewhere; one never uploaded is forgotten, its only copy on that machine.
func TestRmLost(t *testing.T) {
	ctx := t.Context()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem := bucket.NewMem()
	s.vault.Store(&vault{bucket: mem})
	if err := s.store.ensureWorker("w-a", "a"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ id, uid string }{{"stored", "b-stored"}, {"local", "b-local"}} {
		if err := s.store.putBox(api.Box{ID: b.id, Worker: "w-a", State: "asleep", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.putMeta(b.id, boxMeta{project: "p", uid: b.uid}); err != nil {
			t.Fatal(err)
		}
	}
	store := s.boxStore("p", "b-stored", "")
	st := &durable.Store{Bucket: mem}
	m := &durable.Manifest{Created: time.Now(), Files: map[string]*durable.File{}}
	if err := st.PutManifest(ctx, s.master.ProjectKey("p"), durable.ManifestName(store.Prefix, "", 3), m); err != nil {
		t.Fatal(err)
	}
	rm := func(lost bool) error {
		p, _ := json.Marshal(api.MachineRmParams{ID: "w-a", Lost: lost})
		_, err := s.workerRm(ctx, p, nil)
		return err
	}

	left, right := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, _ := yamux.Client(left, cfg)
	defer right.Close()
	s.mu.Lock()
	s.workers["w-a"] = &workerConn{id: "w-a", sess: sess, info: api.Worker{Name: "a"}}
	s.mu.Unlock()
	if err := rm(true); err == nil || !strings.Contains(err.Error(), "isn't lost") {
		t.Fatalf("rm --lost of a connected machine: %v", err)
	}
	s.mu.Lock()
	delete(s.workers, "w-a")
	s.mu.Unlock()
	if err := rm(false); err == nil || !strings.Contains(err.Error(), "rm --lost") {
		t.Fatalf("plain rm of an offline machine: %v", err)
	}
	if err := rm(true); err != nil {
		t.Fatal(err)
	}
	if lost, _ := s.workerLost("w-a"); !lost {
		t.Fatal("the lost machine wasn't removed")
	}
	if b, err := s.store.box("stored"); err != nil || b.Worker != "w-a" {
		t.Fatalf("a box in the bucket: %+v %v, want it kept to restore when used", b, err)
	}
	if _, err := s.store.box("local"); err == nil {
		t.Fatal("a box never uploaded is still listed")
	}
}
