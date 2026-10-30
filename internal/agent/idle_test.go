package agent

import (
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// Typing in a waiting agent's terminal echoes, and that echo isn't the agent
// working; output once you've stopped typing is.
func TestTypingIsNotWorking(t *testing.T) {
	m := newMonitor(nil, nil)
	now := time.Unix(1_800_000_000, 0)
	a := &boxActivity{since: now.Add(-time.Minute)}
	state := func(output, input time.Time) string {
		st := guestproto.Status{Threads: []guestproto.ThreadStatus{{N: 1, Agent: "claude", LastOutput: output.Unix(), LastInput: input.Unix()}}}
		return m.threadStates(a, st, now)[0].State
	}
	if s := state(now.Add(-time.Second), now.Add(-time.Second)); s != api.ThreadWaiting {
		t.Errorf("echo of typing: %s, want waiting", s)
	}
	if s := state(now.Add(-time.Second), now.Add(-10*time.Second)); s != api.ThreadWorking {
		t.Errorf("output after typing stopped: %s, want working", s)
	}
	if s := state(now.Add(-time.Second), time.Unix(0, 0)); s != api.ThreadWorking {
		t.Errorf("output with no typing: %s, want working", s)
	}
}
