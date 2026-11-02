package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Each row of the reconciliation table: a worker's copy is the box at the lead's
// ownership number or a higher one, adopted when unknown, and kept asleep under
// a new name when it lost to a higher number or was removed; nothing is deleted.
func TestReconcile(t *testing.T) {
	ctx := t.Context()
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
	srv, err := yamux.Server(right, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var calls []string
	go rpc.Serve(ctx, srv, rpc.Mux{
		api.BoxSleep: func(_ context.Context, p json.RawMessage, _ *rpc.Out) (any, error) {
			var id api.IDParams
			json.Unmarshal(p, &id)
			calls = append(calls, "sleep "+id.ID)
			return nil, nil
		},
		api.BoxRm: func(_ context.Context, p json.RawMessage, _ *rpc.Out) (any, error) {
			calls = append(calls, "rm")
			return nil, nil
		},
		api.BoxRename: func(_ context.Context, p json.RawMessage, _ *rpc.Out) (any, error) {
			var r api.RenameParams
			json.Unmarshal(p, &r)
			calls = append(calls, "rename "+r.ID+" "+r.NewID)
			if r.Store == nil || strings.Contains(r.Store.Prefix, "b-stale") {
				t.Errorf("%s kept the box's place in the bucket: %+v", r.NewID, r.Store)
			}
			return api.Box{ID: r.NewID, State: "asleep"}, nil
		},
	}, nil)

	for _, id := range []string{"w-old", "w-new", "w-lost"} {
		if err := s.store.ensureWorker(id, id); err != nil {
			t.Fatal(err)
		}
	}
	known := func(id, worker, owner string) {
		t.Helper()
		if err := s.store.putBox(api.Box{ID: id, Worker: worker, State: "asleep", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.putMeta(id, boxMeta{project: "p", uid: "b-" + id}); err != nil {
			t.Fatal(err)
		}
		s.store.setOwner(id, owner)
	}
	known("same", "w-old", "1.3")
	known("missed", "w-new", "1.4") // moved here in a minute the lead's backup missed
	known("stale", "w-new", "1.9")  // moved away while w-old was cut off
	known("behind", "w-old", "1.7") // placed here again since this copy
	known("held", "w-lost", "1.8")  // its machine was lost, and is added again before it was restored
	if err := s.store.setWorkerGone("w-lost"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.recordDeleted("b-gone", "1.2", time.Hour); err != nil {
		t.Fatal(err)
	}
	report := []api.Box{
		{ID: "same", State: "running", Owner: "1.3", Project: "p", UID: "b-same"},
		{ID: "missed", State: "asleep", Owner: "2.1", Project: "p", UID: "b-missed"},
		{ID: "stale", State: "running", Owner: "1.5", Project: "p", UID: "b-stale"},
		{ID: "behind", State: "asleep", Owner: "1.6", Project: "p", UID: "b-behind"},
		{ID: "held", State: "asleep", Owner: "1.8", Project: "p", UID: "b-held"},
		{ID: "gone", State: "asleep", Owner: "1.2", Project: "p", UID: "b-gone"},
		{ID: "unknown", State: "asleep", Owner: "2.2", Project: "p", UID: "b-unknown"},
	}
	w := &workerConn{id: "w-old", sess: sess, info: api.Worker{Name: "old", Boxes: report}, projects: map[string]bool{}}
	keep, _ := s.reconcile(ctx, w, report)
	if err := s.store.syncWorkerBoxes(w.id, keep, nil); err != nil {
		t.Fatal(err)
	}

	where := map[string]string{}
	boxes, _ := s.store.boxes()
	for _, b := range boxes {
		where[b.ID] = b.Worker + " " + b.Owner
	}
	for id, want := range map[string]string{
		"same":            "w-old 1.3",
		"missed":          "w-old 2.1",
		"stale":           "w-new 1.9",
		"stale-returned":  "w-old ",
		"gone-returned":   "w-old ",
		"behind-returned": "w-old ",
		"unknown":         "w-old 2.2",
		"held":            "w-old 1.8",
	} {
		if where[id] != want {
			t.Errorf("%s: %q, want %q", id, where[id], want)
		}
	}
	if _, ok := where["gone"]; ok {
		t.Error("a removed box came back under its name")
	}
	if strings.Join(calls, ", ") != "sleep stale, rename stale stale-returned, rename behind behind-returned, rename gone gone-returned" {
		t.Errorf("calls to the worker: %v", calls)
	}
}
