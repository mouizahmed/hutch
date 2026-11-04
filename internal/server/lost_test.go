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
	"github.com/mouizahmed/hutch/internal/link"
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
	// Said to be down, but it still writes its heartbeat: refused.
	if err := link.PutAlive(ctx, mem, "w-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(api.MachineRmParams{ID: "w-a", Lost: true, Down: true})
	if _, err := s.workerRm(ctx, p, nil); err == nil || !strings.Contains(err.Error(), "isn't down") {
		t.Fatalf("rm --lost --down of a machine still writing to the bucket: %v", err)
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

// Doctor tells a machine that was added but never connected (its link port
// is likely blocked) from one that went offline.
func TestDoctorNeverConnected(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.enrollWorker("w-new", "new", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.store.enrollWorker("w-old", "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.store.workerSeen("w-old", api.Worker{Version: "v0.2.0"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.doctor(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, c := range res.(api.DoctorResult).Checks {
		if c.Name == "state" {
			states[c.Area] = c.Detail
		}
	}
	if !strings.Contains(states["machine new"], "never connected") || !strings.Contains(states["machine old"], "offline since") {
		t.Fatalf("states: %v", states)
	}
}

func TestWorkerHeartbeatSurvivesLeadRestart(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.ensureWorker("worker", "fixture"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-6 * time.Hour)
	if _, err := s.store.db.Exec(`UPDATE workers SET last_seen = ?, version = 'v0.2.4' WHERE id = 'worker'`, old); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if _, err := s.workerMux("worker")[api.WorkerReport](t.Context(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := restarted.store.workers()
	if err != nil || len(ws) != 1 || ws[0].LastSeen.Before(before) {
		t.Fatalf("last seen after restart: %+v, %v", ws, err)
	}
}
