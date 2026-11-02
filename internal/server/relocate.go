package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// workerLost reports whether a worker is gone: removed, or a machine this one
// was restored from.
func (s *Server) workerLost(id string) (bool, error) {
	ws, err := s.store.workers()
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if w.ID == id {
			return w.Gone, nil
		}
	}
	return true, nil
}

func (s *Server) lostWorker(id string) bool {
	lost, err := s.workerLost(id)
	return err == nil && lost
}

// boxWorker returns the worker that has a box. When that worker has been lost,
// the box is restored on another one from its newest snapshot in the bucket.
func (s *Server) boxWorker(ctx context.Context, b api.Box) (*workerConn, api.Box, error) {
	if w, err := s.worker(b.Worker); err == nil {
		return w, b, nil
	}
	mu, _ := s.moving.LoadOrStore(b.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	// Another call may have moved it meanwhile.
	b, err := s.store.box(b.ID)
	if err != nil {
		return nil, b, err
	}
	if w, err := s.worker(b.Worker); err == nil {
		return w, b, nil
	}
	lost, err := s.workerLost(b.Worker)
	if err != nil {
		return nil, b, err
	}
	if !lost {
		return nil, b, fmt.Errorf("%s is on a machine that is offline", b.ID)
	}
	store, err := s.storeOf(b)
	if err != nil {
		return nil, b, err
	}
	owner, gen, err := s.latestSnapshot(ctx, store)
	if err != nil {
		return nil, b, fmt.Errorf("%s can't move from lost worker %s: %w", b.ID, b.Worker, err)
	}
	w, release, err := s.placeWait(ctx, need{memMiB: b.MemMiB, vcpus: b.VCPUs, class: b.Class}, "", b.ID, io.Discard)
	if err != nil {
		return nil, b, err
	}
	defer release()
	s.hostProject(ctx, w, store.Project)
	claimed, err := s.claim(ctx, store, owner, gen)
	if err != nil {
		return nil, b, err
	}
	nb, err := s.callForBox(ctx, w, api.BoxRestore, api.RestoreParams{ID: b.ID, Store: *claimed, Generation: gen})
	if err != nil {
		return nil, b, fmt.Errorf("restore %s on %s: %w", b.ID, w.id, err)
	}
	if err := s.store.setOwner(b.ID, claimed.Owner); err != nil {
		return nil, b, err
	}
	msg := fmt.Sprintf("restored on %s from snapshot %d after worker %s was lost", w.id, gen, b.Worker)
	s.store.event(b.ID, "moved", msg)
	log.Printf("%s: %s", b.ID, msg)
	nb.Project, nb.Kind, nb.Branch, nb.Base, nb.UID, nb.Owner = b.Project, b.Kind, b.Branch, b.Base, b.UID, claimed.Owner
	return w, nb, nil
}

// storeOf is where a box's snapshots are in the bucket.
func (s *Server) storeOf(b api.Box) (*api.Store, error) {
	if b.Kind == api.BoxVersion {
		versions, err := s.store.templates(b.Project)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if v.Box == b.ID {
				return s.versionStore(b.Project, b.Class, v.Version), nil
			}
		}
		if project, version, class, err := s.store.buildVersion(b.ID); err == nil {
			return s.versionStore(project, class, version), nil
		}
	}
	if b.Kind != "" || b.Project == "" {
		return nil, fmt.Errorf("%s isn't stored in the bucket", b.ID)
	}
	uid := b.UID
	if uid == "" {
		uid = b.ID
	}
	return s.boxStore(b.Project, uid, b.Owner), nil
}

// reconcile compares a reconnecting worker's boxes with what the lead knows, by
// ownership number. Its copy of a box is the box when the lead has it there at
// that number, or at a lower one (a move the lead's database missed), and is
// adopted when the lead has never heard of it. A copy that lost to a higher
// number elsewhere, or of a box that was removed, is stale: it's kept asleep as
// a new box, <name>-returned, never deleted. It returns the boxes to record, and
// separately the template versions the worker holds copies of.
func (s *Server) reconcile(ctx context.Context, w *workerConn, boxes []api.Box) (keep []api.Box, copies []string) {
	for _, b := range boxes {
		known, err := s.store.box(b.ID)
		if err == nil && known.Worker != w.id && known.Kind == api.BoxVersion {
			copies = append(copies, b.ID)
			continue
		}
		deleted := false
		if b.UID != "" {
			_, deleted = s.store.deleted(b.UID)
		}
		switch {
		case err != nil && !deleted && !s.store.removed(b.ID):
			if b.Project != "" {
				b.Worker = w.id
				if err := s.store.putBox(b); err != nil {
					log.Printf("worker %s: adopt %s: %v", w.id, b.ID, err)
					continue
				}
				s.store.putMeta(b.ID, boxMeta{project: b.Project, uid: b.UID})
				s.store.setOwner(b.ID, b.Owner)
			}
			s.store.event(b.ID, "adopted", fmt.Sprintf("%s reported it and the lead had no record of it", w.info.Name))
			keep = append(keep, b)
			continue
		case err == nil && known.Kind != "":
			keep = append(keep, b)
			continue
		case err == nil && (known.Worker == w.id && !durable.OwnerLess(b.Owner, known.Owner) || durable.OwnerLess(known.Owner, b.Owner) ||
			b.Owner == known.Owner && s.lostWorker(known.Worker)):
			// The last case: a lost machine added again before its box was
			// restored elsewhere has the box, with work the bucket may lack.
			if b.Owner != known.Owner {
				s.store.setOwner(b.ID, b.Owner)
			}
			keep = append(keep, b)
			continue
		}
		why := "it was removed"
		if err == nil {
			why = fmt.Sprintf("the box is on %s at a higher ownership number", known.Worker)
		}
		r, rerr := s.returned(ctx, w, b)
		if rerr != nil {
			log.Printf("worker %s: keep stale %s: %v", w.id, b.ID, rerr)
			continue
		}
		keep = append(keep, r)
		msg := fmt.Sprintf("%s had an older copy of %s (%s); it's kept asleep as %s", w.info.Name, b.ID, why, r.ID)
		s.store.event(r.ID, "returned", msg)
		s.notify("returned", r.ID, "Old copy kept", msg)
	}
	return keep, copies
}

// returned puts a worker's stale copy of a box to sleep under a name of its own,
// with its own place in the bucket, so it never writes over the box.
func (s *Server) returned(ctx context.Context, w *workerConn, b api.Box) (api.Box, error) {
	if b.State == "running" || b.State == "crashed" {
		if err := rpcCall(ctx, w, api.BoxSleep, api.IDParams{ID: b.ID}); err != nil {
			return b, err
		}
	}
	name := b.ID + "-returned"
	for i := 2; ; i++ {
		if _, err := s.store.box(name); err != nil && !slices.ContainsFunc(w.info.Boxes, func(o api.Box) bool { return o.ID == name }) {
			break
		}
		name = fmt.Sprintf("%s-returned-%d", b.ID, i)
	}
	p := api.RenameParams{ID: b.ID, NewID: name}
	uid := ""
	if b.Project != "" {
		uid = "b-" + randomHex(8)
		p.Store = s.boxStore(b.Project, uid, "")
	}
	var nb api.Box
	if err := rpc.Call(ctx, w.sess, api.BoxRename, p, &nb); err != nil {
		return b, err
	}
	nb.Worker = w.id
	if err := s.store.putBox(nb); err != nil {
		return nb, err
	}
	if b.Project != "" {
		if err := s.store.putMeta(name, boxMeta{project: b.Project, uid: uid}); err != nil {
			return nb, err
		}
	}
	return nb, nil
}

// archiveLoop archives boxes asleep for longer than archive_after: their memory is
// dropped and their disk kept.
func (s *Server) archiveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
		cfg, err := s.settings()
		if err != nil {
			continue
		}
		after := cfg.Duration("archive_after")
		since, err := s.store.statesSince()
		if err != nil {
			continue
		}
		boxes, err := s.store.boxes()
		if err != nil {
			continue
		}
		for _, b := range boxes {
			if b.State != "asleep" || b.Kind != "" || time.Since(since[b.ID]) < after {
				continue
			}
			w, err := s.worker(b.Worker)
			if err != nil {
				continue
			}
			if _, err := s.callForBox(ctx, w, api.BoxArchive, api.IDParams{ID: b.ID}); err != nil {
				log.Printf("archive %s: %v", b.ID, err)
				continue
			}
			s.store.event(b.ID, "archived", fmt.Sprintf("asleep for %s; memory dropped, disk kept", after))
		}
	}
}

// maxAwakeLoop puts boxes to sleep that have been awake for limits.box_max_awake,
// even mid-task: a checkpoint loses nothing, and waking gives a fresh allowance.
func (s *Server) maxAwakeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		v, err := s.limits("limits.box_max_awake")
		if err != nil {
			continue
		}
		max := v.(time.Duration)
		since, err := s.store.statesSince()
		if err != nil {
			continue
		}
		boxes, err := s.store.boxes()
		if err != nil {
			continue
		}
		for _, b := range boxes {
			if b.State != "running" || b.Kind == api.BoxTemplate || time.Since(since[b.ID]) < max {
				continue
			}
			w, err := s.worker(b.Worker)
			if err != nil {
				continue
			}
			if _, err := s.callForBox(ctx, w, api.BoxSleep, api.IDParams{ID: b.ID}); err != nil {
				log.Printf("max awake %s: %v", b.ID, err)
				continue
			}
			msg := fmt.Sprintf("%s was awake for %s (limits.box_max_awake) and was put to sleep; hutch wake %s starts it again", b.ID, max, b.ID)
			s.store.event(b.ID, "force-slept", msg)
			s.notify("force-slept", b.ID, "Box put to sleep", msg)
		}
	}
}

// admitWake waits while limits.awake_boxes, or machine.max_boxes on the box's
// machine, is reached, then counts the box as starting until release. A box
// queued on its full machine for placement.move_queued_after moves to one of its
// class with room; it returns the box as it is then.
func (s *Server) admitWake(ctx context.Context, b api.Box) (api.Box, func(), error) {
	var queued time.Time
	for {
		boxes, err := s.store.boxes()
		if err != nil {
			return b, nil, err
		}
		running, there := 0, 0
		for _, o := range boxes {
			if o.State == "running" {
				running++
				if o.Worker == b.Worker {
					there++
				}
			}
		}
		lim := s.limitInt("limits.awake_boxes")
		cfg, err := s.settingsOn(b.Worker)
		if err != nil {
			return b, nil, err
		}
		max := cfg.Int("machine.max_boxes")
		s.mu.Lock()
		reason, full := "", false
		switch {
		case int64(running+s.starting) >= lim:
			reason = fmt.Sprintf("limits.awake_boxes (%d) reached", lim)
		case max > 0 && int64(there+s.placing[b.Worker]) >= max:
			reason, full = fmt.Sprintf("machine.max_boxes (%d) reached on its machine", max), true
		}
		if reason == "" {
			s.starting++
			s.placing[b.Worker]++
			s.mu.Unlock()
			return b, func() {
				s.mu.Lock()
				s.starting--
				s.placing[b.Worker]--
				s.mu.Unlock()
			}, nil
		}
		s.mu.Unlock()
		if queued.IsZero() {
			queued = time.Now()
			msg := fmt.Sprintf("%s is queued: %s", b.ID, reason)
			s.notify("queued", b.ID, "Box queued", msg)
			s.store.event(b.ID, "queued", msg)
		}
		if after := cfg.Duration("placement.move_queued_after"); full && after > 0 && time.Since(queued) >= after {
			if moved, err := s.moveQueued(ctx, b, after); err == nil {
				b, queued = moved, time.Time{}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return b, nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// moveQueued moves a box whose wake waited on its full machine for after to a
// machine of its class with room, if its newest generation is in the bucket.
func (s *Server) moveQueued(ctx context.Context, b api.Box, after time.Duration) (api.Box, error) {
	store, err := s.storeOf(b)
	if err != nil {
		return b, err
	}
	if have, err := s.storedGeneration(ctx, store); err != nil || have < b.Generation {
		return b, fmt.Errorf("generation %d isn't in the bucket yet", b.Generation)
	}
	r, err := s.move(ctx, b.ID, moveOpts{forWake: true}, io.Discard, io.Discard)
	if err != nil {
		return b, err
	}
	msg := fmt.Sprintf("%s moved from %s to %s: its machine stayed full for %s", b.ID, r.From, r.To, after)
	s.store.event(b.ID, "moved", msg)
	s.notify("moved", b.ID, "Box moved", msg)
	return s.store.box(b.ID)
}

// recoverCrash tells you a box's VM died and wakes it from the snapshot it was
// rolled back to, so its agents carry on from there.
func (s *Server) recoverCrash(workerID string, c api.Crash) {
	if c.State == "failed" {
		msg := fmt.Sprintf("%s's VM died on %s before it had any snapshot; it can't be recovered (its pushed commits are on origin)", c.Box, workerID)
		s.store.event(c.Box, "crashed", msg)
		s.notify("crashed", c.Box, "Box lost", msg)
		return
	}
	msg := fmt.Sprintf("%s's VM died on %s (most likely out of memory); rolled back to its snapshot from %s ago and waking it",
		c.Box, workerID, time.Since(c.Since).Round(time.Second))
	s.store.event(c.Box, "crashed", msg)
	s.notify("crashed", c.Box, "Box rolled back", msg)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := s.awake(ctx, c.Box); err != nil {
		log.Printf("wake %s after its crash: %v", c.Box, err)
	}
}
