package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func (s *Server) boxMove(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.MoveParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	return s.move(ctx, p.ID, p.To, stdout, stderr)
}

// move puts a box on another worker through the bucket: warm (processes and all)
// on a worker of its snapshot class, else cold (its disk, booted; agents' threads come back exited).
// A box that was awake is awake again afterwards.
func (s *Server) move(ctx context.Context, id, to string, stdout, stderr io.Writer) (api.MoveResult, error) {
	var r api.MoveResult
	b, err := s.store.box(id)
	if err != nil {
		return r, err
	}
	if b.Kind != "" {
		return r, fmt.Errorf("%s is a template box; it isn't moved", id)
	}
	store, err := s.storeOf(b)
	if err != nil {
		return r, err
	}
	src, err := s.worker(b.Worker)
	if err != nil {
		return r, err
	}
	if to == src.id {
		return r, fmt.Errorf("%s is already on %s", id, to)
	}
	awake := b.State == "running"
	if awake {
		fmt.Fprintf(stdout, "Snapshotting %s...\n", id)
		if b, err = s.callForBox(ctx, src, api.BoxSleep, api.IDParams{ID: id}); err != nil {
			return r, err
		}
	}
	fmt.Fprintf(stdout, "Waiting for snapshot %d to reach the bucket...\n", b.Generation)
	if err := s.waitStored(ctx, store, b.Generation, 30*time.Minute); err != nil {
		return r, err
	}
	n := need{memMiB: b.MemMiB, vcpus: b.VCPUs, class: b.Class, avoid: src.id}
	w, release, err := s.placeWait(ctx, n, to, id, stderr)
	if err != nil {
		return r, err
	}
	defer func() { release() }()
	s.hostProject(ctx, w, store.Project)
	fmt.Fprintf(stdout, "Restoring on %s...\n", w.info.Name)
	nb, err := s.callForBox(ctx, w, api.BoxRestore, api.RestoreParams{ID: id, Store: *store, Generation: b.Generation})
	if err != nil {
		return r, fmt.Errorf("restore on %s: %w (the box is still on %s)", w.id, err, src.id)
	}
	if err := rpc.Call(ctx, src.sess, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
		fmt.Fprintf(stderr, "warning: removing the old copy on %s: %v (it's removed when that worker reconnects)\n", src.id, err)
	}
	r = api.MoveResult{From: src.id, To: w.id, Kind: "warm"}
	if nb.State == "archived" {
		r.Kind = "cold"
	}
	s.store.event(id, "moved", fmt.Sprintf("%s move from %s to %s", r.Kind, src.id, w.id))
	if awake {
		release()
		release = func() {}
		if _, err := s.awake(ctx, id); err != nil {
			return r, fmt.Errorf("moved, but waking failed: %w", err)
		}
	}
	return r, nil
}

// waitStored waits until the bucket has a box's generation gen.
func (s *Server) waitStored(ctx context.Context, store *api.Store, gen int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if have, err := s.latestGeneration(ctx, store); err == nil && have >= gen {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("snapshot %d didn't reach the bucket within %s", gen, timeout)
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Server) workerDrain(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.DrainParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if err := s.store.setDraining(p.Worker, !p.Undo); err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	if p.Undo {
		fmt.Fprintf(stdout, "%s takes new boxes again.\n", p.Worker)
		return nil, nil
	}
	fmt.Fprintf(stdout, "%s takes no new boxes; moving its awake boxes.\n", p.Worker)
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	for _, b := range boxes {
		if b.Worker != p.Worker || b.State != "running" || b.Kind != "" {
			continue
		}
		if _, err := s.storeOf(b); err != nil {
			fmt.Fprintf(stderr, "skipping %s: %v\n", b.ID, err)
			continue
		}
		r, err := s.move(ctx, b.ID, "", stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", b.ID, err)
			continue
		}
		fmt.Fprintf(stdout, "%s: %s move to %s\n", b.ID, r.Kind, r.To)
	}
	return nil, nil
}

// workerRm forgets an offline worker.
// Its boxes that are in the bucket restore on another worker when used; a worker
// holding any that aren't is kept.
func (s *Server) workerRm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := s.worker(p.ID); err == nil {
		return nil, fmt.Errorf("%s is online; drain it and stop hutchd first", p.ID)
	}
	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.ID != p.ID || w.Gone {
			continue
		}
		boxes, err := s.store.boxes()
		if err != nil {
			return nil, err
		}
		for _, b := range boxes {
			if b.Worker != w.ID {
				continue
			}
			if _, err := s.storeOf(b); err != nil {
				return nil, fmt.Errorf("%s holds %s, which isn't in the bucket; remove it first", w.ID, b.ID)
			}
		}
		s.store.event("", "worker removed", w.Name)
		return nil, s.store.setWorkerGone(w.ID)
	}
	return nil, fmt.Errorf("no worker %s", p.ID)
}

// boxResize changes a box's size with a cold boot: it sleeps (a snapshot), drops
// its memory, takes the new size, and wakes, on another worker if it no longer
// fits its own. Its agents' threads come back exited.
func (s *Server) boxResize(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.ResizeParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	b, err := s.store.box(p.ID)
	if err != nil {
		return nil, err
	}
	if b.Kind != "" {
		return nil, fmt.Errorf("%s is a template box; change the project size instead", p.ID)
	}
	n := need{memMiB: b.MemMiB, vcpus: b.VCPUs}
	if p.MemMiB > 0 {
		n.memMiB = p.MemMiB
	}
	if p.VCPUs > 0 {
		n.vcpus = p.VCPUs
	}
	if err := s.checkFits(ctx, n); err != nil {
		return nil, err
	}
	w, b, err := s.boxWorker(ctx, b)
	if err != nil {
		return nil, err
	}
	awake := b.State == "running"
	if awake {
		fmt.Fprintf(stdout, "Snapshotting %s...\n", p.ID)
		if _, err := s.callForBox(ctx, w, api.BoxSleep, api.IDParams{ID: p.ID}); err != nil {
			return nil, err
		}
	}
	nb, err := s.callForBox(ctx, w, api.BoxResize, p)
	if err != nil {
		return nil, err
	}
	s.store.event(p.ID, "resized", fmt.Sprintf("%d vCPU, %s", nb.VCPUs, mib(nb.MemMiB)))
	fmt.Fprintf(stdout, "%s is now %d vCPU, %s.\n", p.ID, nb.VCPUs, mib(nb.MemMiB))
	if !awake {
		return nb, nil
	}
	// It wakes where it fits: its own worker if there's room, else another.
	if _, release, err := s.place(n, w.id); err == nil {
		release()
	} else {
		fmt.Fprintf(stdout, "It no longer fits %s; moving it.\n", w.info.Name)
		if _, err := s.move(ctx, p.ID, "", stdout, stderr); err != nil {
			return nil, err
		}
		return s.store.box(p.ID)
	}
	fmt.Fprintf(stdout, "Booting %s with its new size...\n", p.ID)
	if _, err := s.awake(ctx, p.ID); err != nil {
		return nil, err
	}
	return s.store.box(p.ID)
}

// relieve moves the working box using the most memory off a worker under memory
// pressure, one move at a time per worker.
func (s *Server) relieve(w *workerConn) {
	s.mu.Lock()
	if s.relieving[w.id] {
		s.mu.Unlock()
		return
	}
	s.relieving[w.id] = true
	peaks := maps.Clone(s.peaks)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.relieving, w.id)
		s.mu.Unlock()
	}()
	boxes, err := s.store.boxes()
	if err != nil {
		return
	}
	var pick api.Box
	for _, b := range boxes {
		if b.Worker != w.id || b.State != "running" || b.Kind != "" {
			continue
		}
		if _, err := s.storeOf(b); err != nil {
			continue
		}
		if peaks[b.ID] > peaks[pick.ID] || pick.ID == "" {
			pick = b
		}
	}
	if pick.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	log.Printf("memory: moving %s off %s", pick.ID, w.info.Name)
	r, err := s.move(ctx, pick.ID, "", io.Discard, io.Discard)
	if err != nil {
		log.Printf("memory: move %s: %v", pick.ID, err)
		return
	}
	msg := fmt.Sprintf("%s moved %s from %s to %s: memory pressure", pick.ID, r.Kind, r.From, r.To)
	s.store.event(pick.ID, "moved", msg)
	s.notify("moved", pick.ID, "Box moved", msg)
}
