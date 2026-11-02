package server

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/mouizahmed/hutch/internal/rpc"
)

// serveWorker is a connected worker answering mux, and ok to the rest of what
// the lead sends it.
func serveWorker(t *testing.T, s *Server, id string, info api.Worker, mux rpc.Mux) {
	t.Helper()
	left, right := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	sess, err := yamux.Client(left, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := yamux.Server(right, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(); srv.Close() })
	ok := func(context.Context, json.RawMessage, *rpc.Out) (any, error) { return nil, nil }
	for _, m := range []string{api.WorkerSettings, api.WorkerKeys} {
		if mux[m] == nil {
			mux[m] = ok
		}
	}
	go rpc.Serve(t.Context(), srv, mux, nil)
	if err := s.store.ensureWorker(id, info.Name); err != nil {
		t.Fatal(err)
	}
	if err := s.store.workerSeen(id, info); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.workers[id] = &workerConn{id: id, sess: sess, info: info, projects: map[string]bool{}}
	s.mu.Unlock()
}

// storedBox is a box asleep on worker with generation gen in the bucket.
func storedBox(t *testing.T, s *Server, mem *bucket.Mem, id, worker string, gen int) *api.Store {
	t.Helper()
	if err := s.store.putBox(api.Box{ID: id, Worker: worker, State: "asleep", Generation: gen, VCPUs: 2, MemMiB: 4096, Class: "intel", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.putMeta(id, boxMeta{project: "p", uid: "b-" + id}); err != nil {
		t.Fatal(err)
	}
	store := s.boxStore("p", "b-"+id, "")
	m := &durable.Manifest{Created: time.Now(), Files: map[string]*durable.File{}}
	if err := (&durable.Store{Bucket: mem}).PutManifest(t.Context(), s.master.ProjectKey("p"), durable.ManifestName(store.Prefix, "", gen), m); err != nil {
		t.Fatal(err)
	}
	return store
}

// A move whose restore fails leaves no manifest at the new ownership number,
// which would otherwise outrank the box's later snapshots where it stays.
func TestFailedMoveUnclaims(t *testing.T) {
	ctx := t.Context()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem := bucket.NewMem()
	s.vault.Store(&vault{bucket: mem})
	info := func(name string) api.Worker {
		return api.Worker{Name: name, Class: "intel", CPUs: 8, MemMiB: 16384}
	}
	serveWorker(t, s, "w-src", info("src"), rpc.Mux{})
	serveWorker(t, s, "w-dst", info("dst"), rpc.Mux{api.BoxRestore: func(context.Context, json.RawMessage, *rpc.Out) (any, error) {
		return nil, errors.New("disk full")
	}})
	store := storedBox(t, s, mem, "x", "w-src", 3)

	if _, err := s.move(ctx, "x", moveOpts{to: "dst"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("move: %v", err)
	}
	objs, _ := mem.List(ctx, store.Prefix+"/")
	if len(objs) != 1 {
		t.Fatalf("after a failed move the bucket has %v", objs)
	}
	if owner, gen, err := s.latestSnapshot(ctx, store); err != nil || owner != "" || gen != 3 {
		t.Fatalf("the newest snapshot: %q %d %v", owner, gen, err)
	}
}

type listFails struct{ *bucket.Mem }

func (listFails) List(context.Context, string) ([]bucket.Object, error) {
	return nil, errors.New("connection reset")
}

// A bucket error during rm --lost changes nothing: a box isn't forgotten as
// never uploaded because the bucket couldn't be read.
func TestLoseStopsOnBucketError(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem := bucket.NewMem()
	if err := s.store.ensureWorker("w-a", "a"); err != nil {
		t.Fatal(err)
	}
	storedBox(t, s, mem, "x", "w-a", 2)
	s.vault.Store(&vault{bucket: listFails{mem}})
	p, _ := json.Marshal(api.MachineRmParams{ID: "w-a", Lost: true})
	if _, err := s.workerRm(t.Context(), p, nil); err == nil || !strings.Contains(err.Error(), "reading the bucket") {
		t.Fatalf("rm --lost with the bucket failing: %v", err)
	}
	if _, err := s.store.box("x"); err != nil {
		t.Fatal("the box was forgotten")
	}
	if lost, _ := s.workerLost("w-a"); lost {
		t.Fatal("the machine was removed")
	}
}

// After a restore or handover, the first claim waits for this lead's term, so
// it's never 0.n, below every ownership number already in the bucket.
func TestClaimAfterRestoreHasATerm(t *testing.T) {
	ctx := t.Context()
	mem := bucket.NewMem()
	s := leadOn(t, mem, nil, "w-a")
	if err := s.store.setTerm(0); err != nil {
		t.Fatal(err)
	}
	if err := s.store.ensureWorker("w-a", "a"); err != nil {
		t.Fatal(err)
	}
	store := storedBox(t, s, mem, "x", "w-a", 4)
	claimed, err := s.claim(ctx, store, "", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(claimed.Owner, "1.") {
		t.Fatalf("claimed %q at term %d", claimed.Owner, s.store.term())
	}
}

// A restore makes the dead lead, which the lead record names, a lost machine
// even when it was linked (a lead handed over to), with its awake boxes asleep.
func TestRestoredLosesTheLead(t *testing.T) {
	ctx := t.Context()
	mem := bucket.NewMem()
	s := leadOn(t, mem, nil, "w-lead")
	l, _ := s.store.link()
	for _, id := range []string{"w-lead", "w-other"} {
		if err := s.store.enrollWorker(id, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := link.PutRecord(ctx, mem, l.ca, link.Record{Term: 1, Address: "lead.example:7443", WorkerID: "w-lead"}, false); err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ id, worker string }{{"on-lead", "w-lead"}, {"on-other", "w-other"}} {
		if err := s.store.putBox(api.Box{ID: b.id, Worker: b.worker, State: "running", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored(ctx, s.store, mem, ""); err != nil {
		t.Fatal(err)
	}
	if lost, _ := s.workerLost("w-lead"); !lost {
		t.Fatal("the dead lead isn't lost")
	}
	if lost, _ := s.workerLost("w-other"); lost {
		t.Fatal("a linked worker was lost")
	}
	if b, _ := s.store.box("on-lead"); b.State != "asleep" {
		t.Fatalf("the dead lead's box: %s", b.State)
	}
	if b, _ := s.store.box("on-other"); b.State != "running" {
		t.Fatalf("a worker's box: %s", b.State)
	}
	if s.store.term() != 0 {
		t.Fatal("the term wasn't left to claim")
	}
}
