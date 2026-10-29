package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/wire"
)

// hub sends clients what changed: after any write or report the server lists
// boxes and workers again and sends the differences from the last list.
type hub struct {
	kick chan struct{}

	mu      sync.Mutex
	boxes   map[string]api.Box // with their threads
	workers map[string]api.Worker
	subs    map[chan api.Event]bool
}

func newHub() *hub {
	return &hub{kick: make(chan struct{}, 1), boxes: map[string]api.Box{}, workers: map[string]api.Worker{},
		subs: map[chan api.Event]bool{}}
}

// changed asks for a new list; changes made while one is taken are folded into
// the next.
func (s *Server) changed() {
	select {
	case s.events.kick <- struct{}{}:
	default:
	}
}

func (s *Server) publish() {
	for range s.events.kick {
		ctx := context.Background()
		boxes, err := s.boxList(ctx, nil, nil)
		if err != nil {
			continue
		}
		workers, err := s.workerList(ctx, nil, nil)
		if err != nil {
			continue
		}
		s.events.update(boxes.([]api.Box), workers.([]api.Worker))
	}
}

func (h *hub) update(boxes []api.Box, workers []api.Worker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []api.Event
	next := map[string]api.Box{}
	for _, b := range boxes {
		next[b.ID] = b
		old, had := h.boxes[b.ID]
		if !had || !same(withoutThreads(old), withoutThreads(b)) {
			bb := withoutThreads(b)
			out = append(out, api.Event{Type: api.EventBox, Box: &bb})
		}
		was := map[int]api.Thread{}
		for _, t := range old.Threads {
			was[t.N] = t
		}
		for _, t := range b.Threads {
			if prev, ok := was[t.N]; !ok || !same(prev, t) {
				out = append(out, api.Event{Type: api.EventThread, Thread: &t})
			}
			delete(was, t.N)
		}
		for n := range was {
			out = append(out, api.Event{Type: api.EventThreadRemoved, ID: b.ID, N: n})
		}
	}
	for id := range h.boxes {
		if _, ok := next[id]; !ok {
			out = append(out, api.Event{Type: api.EventBoxRemoved, ID: id})
		}
	}
	nextWorkers := map[string]api.Worker{}
	for _, w := range workers {
		nextWorkers[w.ID] = w
		if old, had := h.workers[w.ID]; !had || !same(old, w) {
			out = append(out, api.Event{Type: api.EventWorker, Worker: &w})
		}
	}
	for id := range h.workers {
		if _, ok := nextWorkers[id]; !ok {
			out = append(out, api.Event{Type: api.EventWorkerRemoved, ID: id})
		}
	}
	h.boxes, h.workers = next, nextWorkers
	for ch := range h.subs {
		for _, e := range out {
			select {
			case ch <- e:
			default:
				// Too far behind: it ends, and reconnects to a fresh start.
				close(ch)
				delete(h.subs, ch)
			}
			if !h.subs[ch] {
				break
			}
		}
	}
}

// subscribe returns a channel of changes, and the current state as events to
// send first.
func (h *hub) subscribe() (chan api.Event, []api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var start []api.Event
	for _, w := range h.workers {
		start = append(start, api.Event{Type: api.EventWorker, Worker: &w})
	}
	for _, b := range h.boxes {
		bb := withoutThreads(b)
		start = append(start, api.Event{Type: api.EventBox, Box: &bb})
		for _, t := range b.Threads {
			start = append(start, api.Event{Type: api.EventThread, Thread: &t})
		}
	}
	start = append(start, api.Event{Type: api.EventSynced})
	ch := make(chan api.Event, 4096)
	h.subs[ch] = true
	return ch, start
}

func (h *hub) unsubscribe(ch chan api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[ch] {
		close(ch)
		delete(h.subs, ch)
	}
}

func (s *Server) eventStream(ctx context.Context, _ json.RawMessage, conn net.Conn) error {
	ch, start := s.events.subscribe()
	defer s.events.unsubscribe(ch)
	for _, e := range start {
		if err := wire.Write(conn, e); err != nil {
			return nil
		}
	}
	gone := make(chan struct{})
	go func() {
		var b [1]byte
		conn.Read(b[:])
		close(gone)
	}()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return fmt.Errorf("events: fell behind")
			}
			if err := wire.Write(conn, e); err != nil {
				return nil
			}
		case <-gone:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func withoutThreads(b api.Box) api.Box {
	b.Threads = nil
	return b
}

func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
