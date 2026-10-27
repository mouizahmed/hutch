package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
)

// workerLost reports whether a worker has been offline for worker.lost_after.
func (s *Server) workerLost(id string) (bool, time.Duration, error) {
	cfg, err := s.settings()
	if err != nil {
		return false, 0, err
	}
	after := cfg.Duration("worker.lost_after")
	ws, err := s.store.workers()
	if err != nil {
		return false, after, err
	}
	for _, w := range ws {
		if w.ID == id {
			return w.Gone || time.Since(w.LastSeen) >= after, after, nil
		}
	}
	return true, after, nil
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
	if w, err := s.wakeStopped(ctx, b.Worker); err != nil || w != nil {
		return w, b, err
	}
	lost, after, err := s.workerLost(b.Worker)
	if err != nil {
		return nil, b, err
	}
	if !lost {
		return nil, b, fmt.Errorf("worker %s is offline; %s moves to another worker once it has been offline for %s (worker.lost_after)", b.Worker, b.ID, after)
	}
	store, err := s.storeOf(b)
	if err != nil {
		return nil, b, err
	}
	gen, err := s.latestGeneration(ctx, store)
	if err != nil {
		return nil, b, fmt.Errorf("%s can't move from lost worker %s: %w", b.ID, b.Worker, err)
	}
	w, release, err := s.placeWait(ctx, need{memMiB: b.MemMiB, vcpus: b.VCPUs, class: b.Class}, "", b.ID, io.Discard)
	if err != nil {
		return nil, b, err
	}
	defer release()
	s.hostProject(ctx, w, store.Project)
	nb, err := s.callForBox(ctx, w, api.BoxRestore, api.RestoreParams{ID: b.ID, Store: *store, Generation: gen})
	if err != nil {
		return nil, b, fmt.Errorf("restore %s on %s: %w", b.ID, w.id, err)
	}
	msg := fmt.Sprintf("restored on %s from snapshot %d after worker %s was lost", w.id, gen, b.Worker)
	s.store.event(b.ID, "moved", msg)
	log.Printf("%s: %s", b.ID, msg)
	nb.Project, nb.Kind, nb.Branch, nb.Base = b.Project, b.Kind, b.Branch, b.Base
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
	}
	if b.Kind != "" || b.Project == "" {
		return nil, fmt.Errorf("%s isn't stored in the bucket", b.ID)
	}
	return s.boxStore(b.Project, b.ID), nil
}

// dropStale removes from a reconnecting worker the boxes that moved to another
// worker, or were removed, while it was gone. It returns the rest of its boxes, and
// separately the template versions it holds copies of.
func (s *Server) dropStale(ctx context.Context, w *workerConn, boxes []api.Box) (keep []api.Box, copies []string) {
	for _, b := range boxes {
		known, err := s.store.box(b.ID)
		if err == nil && known.Worker != w.id && known.Kind == api.BoxVersion {
			copies = append(copies, b.ID)
			continue
		}
		if err != nil && s.store.removed(b.ID) {
			known.Worker = "nowhere: it was removed"
		} else if err != nil || known.Worker == w.id {
			keep = append(keep, b)
			continue
		}
		if err := rpcCall(ctx, w, api.BoxRm, api.IDParams{ID: b.ID}); err != nil {
			log.Printf("worker %s: remove stale %s: %v", w.id, b.ID, err)
			continue
		}
		s.store.event(b.ID, "stale", fmt.Sprintf("removed the old copy on %s; the box is on %s", w.id, known.Worker))
		log.Printf("worker %s: removed stale copy of %s (now on %s)", w.id, b.ID, known.Worker)
	}
	return keep, copies
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

// admitWake waits while limits.awake_boxes is reached, then counts the box as
// starting until release.
func (s *Server) admitWake(ctx context.Context, box string) (func(), error) {
	notified := false
	for {
		boxes, err := s.store.boxes()
		if err != nil {
			return nil, err
		}
		running := 0
		for _, b := range boxes {
			if b.State == "running" {
				running++
			}
		}
		lim := s.limitInt("limits.awake_boxes")
		s.mu.Lock()
		if int64(running+s.starting) < lim {
			s.starting++
			s.mu.Unlock()
			return func() {
				s.mu.Lock()
				s.starting--
				s.mu.Unlock()
			}, nil
		}
		s.mu.Unlock()
		if !notified {
			msg := fmt.Sprintf("%s is queued: limits.awake_boxes (%d) reached", box, lim)
			s.notify("queued", box, "Box queued", msg)
			s.store.event(box, "queued", msg)
			notified = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// resumeThreads restarts a box's agents after a cold boot, each in its own tmux
// window and session, through its adapter's resume command.
func (s *Server) resumeThreads(ctx context.Context, w *workerConn, box string) {
	threads, err := s.store.threads()
	if err != nil {
		return
	}
	set, err := s.adapters()
	if err != nil {
		return
	}
	started := false
	for _, t := range threads[box] {
		if t.SessionID == "" {
			continue
		}
		_, autonomy, err := s.store.thread(box, t.N)
		if err != nil {
			continue
		}
		argv, err := set.ResumeArgv(t.Agent, autonomy, t.SessionID)
		if err != nil {
			continue
		}
		target := fmt.Sprintf("%s:%d", tmuxSession, t.N)
		var cmd []string
		if started {
			cmd = append([]string{"tmux", "new-window", "-d", "-t", target, "-n", t.Agent, "-c", workspace, "--"}, argv...)
		} else {
			cmd = append([]string{"tmux", "new-session", "-d", "-s", tmuxSession, "-x", "200", "-y", "50", "-n", t.Agent, "-c", workspace,
				"-P", "-F", "#{window_index}", "--"}, argv...)
		}
		out, code, err := s.boxRun(ctx, w, box, cmd...)
		if err != nil || code != 0 {
			log.Printf("resume %s/%d: exit %d %v", box, t.N, code, err)
			continue
		}
		if !started {
			if idx, _ := strconv.Atoi(strings.TrimSpace(out)); idx != t.N {
				s.boxRun(ctx, w, box, "tmux", "move-window", "-s", fmt.Sprintf("%s:%d", tmuxSession, idx), "-t", target)
			}
			started = true
		}
		s.store.event(box, api.ThreadResume, strconv.Itoa(t.N))
	}
}
