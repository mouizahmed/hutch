package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
)

func TestThreadNotificationsRequireNewAgentEvents(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "notifications.db"), durable.Key{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	if err := st.ensureWorker("worker", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := st.putBox(api.Box{ID: "box", Worker: "worker", State: "running", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, threadStates: map[string]map[int]api.ThreadState{}}
	now := time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC)
	thread := api.ThreadState{N: 1, Agent: "claude", State: api.ThreadWaiting, Bell: now, Created: now}
	report := func(want int) {
		t.Helper()
		if alerts := s.applyThreads("box", true, []api.ThreadState{thread}); len(alerts) != want {
			t.Fatalf("alerts=%d; want %d for %+v", len(alerts), want, thread)
		}
	}
	report(0) // The first report establishes the baseline, including old bells.
	for minute := 1; minute <= 24*60; minute += 5 {
		thread.State, thread.LastOutput = api.ThreadWorking, now.Add(time.Duration(minute)*time.Minute)
		report(0)
		thread.State = api.ThreadWaiting
		report(0) // Redrawing and then becoming quiet never notify, at any hour.
	}
	thread.Bell = thread.LastOutput.Add(time.Minute)
	report(1) // A real bell still notifies, including overnight.
	report(0)
	thread.Viewed, thread.Bell = true, thread.Bell.Add(time.Minute)
	report(0)
	thread.Viewed = false
	report(0) // Leaving the view must not replay its already-observed bell.
	thread.State = api.ThreadExited
	report(1)
	report(0)
	delete(s.threadStates, "box") // Daemon restart: old events are a baseline.
	report(0)
	s.applyThreads("box", false, nil)
	report(0) // Wake: old exit/bell must not replay.
	thread.Agent, thread.State = "", api.ThreadWorking
	report(0)
	thread.State, thread.Bell = api.ThreadExited, thread.Bell.Add(time.Minute)
	report(0) // Shells and command terminals do not send agent notifications.
}
