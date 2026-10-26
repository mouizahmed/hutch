package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// on a worker of its snapshot class, else cold (its disk, booted, agents resumed).
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
