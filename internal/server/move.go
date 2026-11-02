package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func (s *Server) boxMove(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.MoveParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	return s.move(ctx, p.ID, moveOpts{to: p.To, wait: true}, stdout, stderr)
}

// moveOpts: to is the machine to move to, else the scheduler's choice. With
// wait, a move queues until a machine has room; without, no room is an error and
// the box stays where it is. With wake, the box is awake afterwards even if it
// was asleep. forWake is for a wake that waits: only a machine of its class with
// room will do, and the caller wakes it.
type moveOpts struct {
	to                  string
	wait, wake, forWake bool
}

// move puts a box on another worker through the bucket: warm (processes and all)
// on a worker of its snapshot class, else cold (its disk, booted; agents' threads
// come back exited). A box that was awake is awake again afterwards.
func (s *Server) move(ctx context.Context, id string, o moveOpts, stdout, stderr io.Writer) (api.MoveResult, error) {
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
	if o.to == src.id || o.to == src.info.Name {
		return r, fmt.Errorf("%s is already on %s", id, src.info.Name)
	}
	awake := b.State == "running"
	n := need{memMiB: b.MemMiB, vcpus: b.VCPUs, class: b.Class, avoid: src.id, asleep: !awake && !o.wake && !o.forWake, strict: o.forWake}
	var w *workerConn
	var release func()
	if o.wait {
		w, release, err = s.placeWait(ctx, n, o.to, id, stderr)
	} else {
		w, release, err = s.place(n, o.to)
	}
	if err != nil {
		return r, err
	}
	defer func() { release() }()
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
	s.hostProject(ctx, w, store.Project)
	claimed, err := s.claim(ctx, store, store.Owner, b.Generation)
	if err != nil {
		return r, err
	}
	fmt.Fprintf(stdout, "Restoring on %s...\n", w.info.Name)
	nb, err := s.callForBox(ctx, w, api.BoxRestore, api.RestoreParams{ID: id, Store: *claimed, Generation: b.Generation})
	if err != nil {
		return r, fmt.Errorf("restore on %s: %w (the box is still on %s)", w.info.Name, err, src.info.Name)
	}
	if err := s.store.setOwner(id, claimed.Owner); err != nil {
		return r, err
	}
	if err := rpc.Call(ctx, src.sess, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
		fmt.Fprintf(stderr, "warning: removing the old copy on %s: %v (it's kept asleep as %s-returned when that machine reconnects)\n", src.info.Name, err, id)
	}
	r = api.MoveResult{From: src.info.Name, To: w.info.Name, Kind: "warm"}
	if nb.State == "archived" {
		r.Kind = "cold"
	}
	s.store.event(id, "moved", fmt.Sprintf("%s move from %s to %s", r.Kind, r.From, r.To))
	if awake || o.wake {
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
		if have, err := s.storedGeneration(ctx, store); err == nil && have >= gen {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("snapshot %d didn't reach the bucket within %s", gen, timeout)
		case <-time.After(3 * time.Second):
		}
	}
}

// workerRm forgets a worker and revokes its link certificate. A connected one is
// drained first: each box moves to another machine (warm within its class, cold
// across), and one that can't stops the drain where it is. A lost one (the CLI
// fenced it first, or you said it's down) has its boxes come back elsewhere from
// the bucket.
func (s *Server) workerRm(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.MachineRmParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	local := s.local
	s.mu.Unlock()
	if p.ID == local {
		return nil, errors.New("that's the lead's own machine: it can't be removed")
	}
	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ws, func(w api.Worker) bool { return w.ID == p.ID && !w.Gone })
	if i < 0 {
		return nil, fmt.Errorf("no worker %s", p.ID)
	}
	w := ws[i]
	_, err = s.worker(w.ID)
	switch online := err == nil; {
	case online && p.Lost:
		return nil, fmt.Errorf("%s is connected, so it isn't lost: hutch machine rm %s moves its boxes off it", w.Name, w.Name)
	case online:
		if err := s.drain(ctx, w, out); err != nil {
			return nil, err
		}
	case !p.Lost:
		return nil, fmt.Errorf("%s is offline, so its boxes can't move off it: wait for it, or if it's gone for good, hutch machine rm --lost %s brings them back elsewhere from the bucket", w.Name, w.Name)
	default:
		if err := s.lose(ctx, w, out); err != nil {
			return nil, err
		}
	}
	s.store.event("", "worker removed", w.Name)
	if err := s.store.setWorkerGone(w.ID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if c := s.workers[w.ID]; c != nil {
		c.sess.Close()
	}
	s.mu.Unlock()
	s.wakeLink()
	return nil, nil
}

// drain moves every box off a worker. It takes no new boxes meanwhile; a box
// that can't move stops the drain, and the worker takes boxes again.
func (s *Server) drain(ctx context.Context, w api.Worker, out *rpc.Out) (err error) {
	stdout, stderr := out.Writers()
	if err := s.store.setDraining(w.ID, true); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			s.store.setDraining(w.ID, false)
		}
	}()
	boxes, err := s.store.boxes()
	if err != nil {
		return err
	}
	for _, b := range boxes {
		if b.Worker != w.ID {
			continue
		}
		stop := func(why string) error {
			return fmt.Errorf("%s %s, so %s isn't removed; it takes new boxes again", b.ID, why, w.Name)
		}
		switch b.Kind {
		case api.BoxVersion:
			// Other machines copy a template version from the bucket.
			store, err := s.storeOf(b)
			if err != nil {
				return stop(err.Error())
			}
			if err := s.waitStored(ctx, store, b.Generation, 30*time.Minute); err != nil {
				return stop("(a template version) isn't in the bucket: " + err.Error())
			}
			continue
		case api.BoxTemplate:
			return stop(fmt.Sprintf("is %s's template box, which isn't kept in the bucket: remove it first (hutch rm %s)", b.Project, b.ID))
		}
		r, err := s.move(ctx, b.ID, moveOpts{}, stdout, stderr)
		if err != nil {
			return stop("can't move: " + err.Error())
		}
		fmt.Fprintf(stdout, "%s: %s move to %s\n", b.ID, r.Kind, r.To)
	}
	return nil
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
		if _, err := s.move(ctx, p.ID, moveOpts{wait: true, wake: true}, stdout, stderr); err != nil {
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
	r, err := s.move(ctx, pick.ID, moveOpts{}, io.Discard, io.Discard)
	if err != nil {
		log.Printf("memory: move %s: %v", pick.ID, err)
		return
	}
	msg := fmt.Sprintf("%s moved %s from %s to %s: memory pressure", pick.ID, r.Kind, r.From, r.To)
	s.store.event(pick.ID, "moved", msg)
	s.notify("moved", pick.ID, "Box moved", msg)
}

// lose gives up a lost worker's boxes. Each in the bucket comes back elsewhere
// from its newest snapshot, at a raised ownership number: the awake ones at
// once, the rest when used. One never uploaded is forgotten here; its only copy
// is on that machine, and returns as <box>-returned if the machine is added again.
func (s *Server) lose(ctx context.Context, w api.Worker, out *rpc.Out) error {
	stdout, _ := out.Writers()
	boxes, err := s.store.boxes()
	if err != nil {
		return err
	}
	var wake []string
	for _, b := range boxes {
		if b.Worker != w.ID {
			continue
		}
		if b.Kind == api.BoxVersion {
			continue // other machines copy it from the bucket
		}
		store, err := s.storeOf(b)
		var at time.Time
		if err == nil {
			at, err = s.snapshotTime(ctx, store)
		}
		if err != nil {
			if err := s.store.deleteBox(b.ID, "lost with "+w.Name); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s: lost, it never reached the bucket; its only copy is on %s\n", b.ID, w.Name)
			continue
		}
		age := time.Since(at).Round(time.Second)
		if b.State == "running" {
			wake = append(wake, b.ID)
			fmt.Fprintf(stdout, "%s: back elsewhere now, from its snapshot of %s ago (work since then is lost)\n", b.ID, age)
		} else {
			fmt.Fprintf(stdout, "%s: back elsewhere when you use it, from its snapshot of %s ago\n", b.ID, age)
		}
		b.State = "asleep"
		if err := s.store.putBox(b); err != nil {
			return err
		}
		s.store.event(b.ID, "lost", fmt.Sprintf("%s was lost; restored from the bucket when used, from its snapshot of %s ago", w.Name, age))
	}
	if err := s.store.setWorkerGone(w.ID); err != nil {
		return err
	}
	for _, id := range wake {
		go func() {
			if _, err := s.awake(context.WithoutCancel(ctx), id); err != nil {
				log.Printf("%s: restore after %s was lost: %v", id, w.Name, err)
			}
		}()
	}
	return nil
}

// snapshotTime is when the newest snapshot of a box in the bucket was written.
func (s *Server) snapshotTime(ctx context.Context, store *api.Store) (time.Time, error) {
	owner, gen, err := s.latestSnapshot(ctx, store)
	if err != nil {
		return time.Time{}, err
	}
	v := s.vault.Load()
	if v == nil {
		return time.Time{}, errNoBucket
	}
	o, err := v.bucket.Head(ctx, durable.ManifestName(store.Prefix, owner, gen))
	return o.Modified, err
}
