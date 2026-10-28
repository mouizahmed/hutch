package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mouizahmed/hutch/internal/config"
)

// limitError means a limit stops a box from starting now; it waits.
type limitError struct {
	key   string
	value int64
}

func (e *limitError) Error() string { return fmt.Sprintf("%s (%d) reached", e.key, e.value) }

// limits resolves a limits.* key: yours, capped by the machine's when it sets one.
func (s *Server) limits(key string) (any, error) {
	cfg, err := s.settings()
	if err != nil {
		return nil, err
	}
	v, err := cfg.Get(key)
	if err != nil {
		return nil, err
	}
	d, err := s.store.config("machine")
	if err != nil {
		return nil, err
	}
	doc, err := config.Parse(config.Machine, "machine config", []byte(d.Content))
	if err != nil {
		return nil, err
	}
	sv, err := config.New(doc).Get(key)
	if err != nil || sv.Layer != config.Machine {
		return v.Value, err
	}
	switch a := v.Value.(type) {
	case int64:
		return min(a, sv.Value.(int64)), nil
	case time.Duration:
		return min(a, sv.Value.(time.Duration)), nil
	}
	return v.Value, nil
}

func (s *Server) limitInt(key string) int64 {
	v, err := s.limits(key)
	if err != nil {
		return 0
	}
	return v.(int64)
}

// placeWait places a box, waiting (queued, with a notification) while a limit is
// reached or the workers are full. It returns when the box can start, or ctx ends.
func (s *Server) placeWait(ctx context.Context, n need, requested, box string, out io.Writer) (*workerConn, func(), error) {
	reason := ""
	for {
		w, release, err := s.place(n, requested)
		if err == nil {
			return w, release, nil
		}
		var lim *limitError
		if requested != "" || (!errors.Is(err, errNoRoom) && !errors.As(err, &lim)) {
			return nil, nil, err
		}
		if err.Error() != reason {
			if reason == "" {
				s.notify("queued", box, "Box queued", fmt.Sprintf("%s is queued: %v", box, err))
			}
			reason = err.Error()
			fmt.Fprintf(out, "Queued: %v. %s starts when there's room (Ctrl-C to give up).\n", err, box)
			s.store.event(box, "queued", reason)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

var errNoRoom = errors.New("the machine has no room")
