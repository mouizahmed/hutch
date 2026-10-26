package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/provider/fake"
)

// Teardown stops and destroys real instances: it must never stop a worker with
// work on it, never destroy one holding a box the bucket doesn't have, and keep
// the stopped pool at its size.
func TestTeardown(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir(), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.putConfig("server", 0, `version = 1
providers.aws.worker_type = "w.large"
providers.teardown_after = "1m"
providers.stopped_pool = 2
`); err != nil {
		t.Fatal(err)
	}
	p := fake.New()
	f := &fleet{p: p}
	now := time.Now()
	worker := func(id string, state provider.NodeState, connected bool, uploading int, stopped time.Time) {
		p.Add("i-"+id, "w.large", state)
		s.store.addWorker(id, id, "i-"+id)
		if !stopped.IsZero() {
			s.store.setWorkerStopped(id, stopped)
		}
		if connected {
			s.workers[id] = &workerConn{id: id, info: api.Worker{ID: id, MemMiB: 8192}, uploading: uploading}
		}
	}
	worker("busy", provider.Running, true, 0, time.Time{})
	worker("empty", provider.Running, true, 0, time.Time{})
	worker("uploading", provider.Running, true, 1, time.Time{})
	worker("unseen", provider.Running, false, 0, time.Time{})
	worker("stopped-new", provider.Stopped, false, 0, now.Add(-time.Hour))
	worker("stopped-old", provider.Stopped, false, 0, now.Add(-2*time.Hour))
	worker("stopped-template", provider.Stopped, false, 0, now.Add(-3*time.Hour))
	s.store.putBox(api.Box{ID: "work", Worker: "busy", State: "running", MemMiB: 4096, Created: now})
	s.store.putBox(api.Box{ID: "app-template", Worker: "stopped-template", State: "asleep", Created: now})
	s.store.putMeta("app-template", boxMeta{project: "p", kind: api.BoxTemplate})

	long := now.Add(-2 * time.Minute)
	empty := map[string]time.Time{"busy": long, "empty": long, "uploading": long, "unseen": long}
	if err := s.teardown(context.Background(), f, empty); err != nil {
		t.Fatal(err)
	}
	want := []string{"destroy i-stopped-old", "stop i-empty"}
	slices.Sort(p.Calls)
	if !slices.Equal(p.Calls, want) {
		t.Fatalf("calls %v, want %v", p.Calls, want)
	}
}
