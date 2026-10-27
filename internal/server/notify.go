package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
)

// notify runs the user's notify command for an event, if they set one.
func (s *Server) notify(event, box, title, message string) {
	cfg, err := s.settings()
	if err != nil {
		return
	}
	cmd := cfg.String("notify.command")
	if cmd == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Env = append(os.Environ(), "HUTCH_EVENT="+event, "HUTCH_BOX="+box, "HUTCH_TITLE="+title, "HUTCH_MESSAGE="+message)
		if out, err := c.CombinedOutput(); err != nil {
			log.Printf("notify %s: %v: %s", event, err, out)
		}
	}()
}

// threadWatch is a thread's notification state: whether it has worked since it
// last notified, and since when it has stopped.
type threadWatch struct {
	worked  bool
	stopped time.Time
}

// watchThreads notifies once when a thread that was working has been waiting or
// exited for notify.waiting_after, so a short pause between steps doesn't.
func (s *Server) watchThreads(box string, states map[int]api.ThreadState) {
	cfg, err := s.settings()
	if err != nil || len(states) == 0 {
		return
	}
	after := cfg.Duration("notify.waiting_after")
	now := time.Now()
	var due []api.ThreadState
	s.mu.Lock()
	for n, t := range states {
		key := fmt.Sprintf("%s/%d", box, n)
		w := s.threadWatch[key]
		if w == nil {
			w = &threadWatch{}
			s.threadWatch[key] = w
		}
		switch t.State {
		case api.ThreadWorking:
			w.worked, w.stopped = true, time.Time{}
		case api.ThreadWaiting, api.ThreadExited:
			if !w.worked {
				continue
			}
			if w.stopped.IsZero() {
				w.stopped = now
			}
			if now.Sub(w.stopped) >= after {
				w.worked = false
				due = append(due, t)
			}
		}
	}
	s.mu.Unlock()
	for _, t := range due {
		title := t.Title
		if stored, _, err := s.store.thread(box, t.N); err == nil && (title == "" || stored.Renamed) {
			title = stored.Title
		}
		if t.State == api.ThreadExited {
			s.notify("exited", box, "Agent exited", fmt.Sprintf("%s/%d exited: %s", box, t.N, title))
		} else {
			s.notify("waiting", box, "Agent waiting", fmt.Sprintf("%s/%d is waiting for you: %s", box, t.N, title))
		}
	}
}
