package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
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
		if err := runNotify(cmd, event, box, title, message); err != nil {
			log.Printf("notify %s: %v", event, err)
		}
	}()
}

func runNotify(cmd, event, box, title, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(), "HUTCH_EVENT="+event, "HUTCH_BOX="+box, "HUTCH_TITLE="+title, "HUTCH_MESSAGE="+message)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// notifyTest runs notify.command now and reports how it went.
func (s *Server) notifyTest(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	cfg, err := s.settings()
	if err != nil {
		return nil, err
	}
	cmd := cfg.String("notify.command")
	if cmd == "" {
		return nil, errors.New("notify.command is empty")
	}
	return nil, runNotify(cmd, "test", "", "Hutch", "Notifications from your machine work.")
}

// alert tells you an agent wants you: the user's notify command, and each TUI.
func (s *Server) alert(t api.Thread) {
	s.events.send(api.Event{Type: api.EventAlert, Thread: &t})
	if t.State == api.ThreadExited {
		s.notify("exited", t.Box, "Agent exited", fmt.Sprintf("%s/%d exited: %s", t.Box, t.N, t.Title))
	} else {
		s.notify("bell", t.Box, "Agent wants you", fmt.Sprintf("%s/%d: %s", t.Box, t.N, t.Title))
	}
}
