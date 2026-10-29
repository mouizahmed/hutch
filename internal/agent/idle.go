//go:build linux

package agent

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/worker"
)

type boxActivity struct {
	lastActive time.Time
	cpu        time.Duration
	sampled    time.Time
	// For memory pressure: recent memory samples and the latest, whether a
	// thread is working or a terminal attached, and whether its balloon is inflated.
	samples   []memSample
	memMiB    int
	working   bool
	attached  bool
	reclaimed bool
	// Each thread's last output that wasn't a redraw.
	output map[int]time.Time
}

// monitor checks running boxes, puts idle ones to sleep, and reports to the server
// when connected. It decides locally, so boxes still sleep while the server is down.
type monitor struct {
	w        *worker.Worker
	uploads  *uploader
	settings atomic.Pointer[api.WorkerSettingsParams] // from the server
	defaults worker.Idle                              // for boxes created without a policy
	kicks    chan struct{}
	seen     map[string]*boxActivity
}

func newMonitor(w *worker.Worker, u *uploader) *monitor {
	d := config.New()
	m := &monitor{w: w, uploads: u, kicks: make(chan struct{}, 1), seen: map[string]*boxActivity{},
		defaults: worker.Idle{Timeout: d.Duration("idle_timeout"), CPUThreshold: d.Percent("idle.cpu_threshold")}}
	// Defaults until the server sends its settings, which may never come if the
	// server can't resolve its config.
	p := api.NewWorkerSettings(d)
	m.settings.Store(&p)
	return m
}

// kick asks for a check and report now.
func (m *monitor) kick() {
	select {
	case m.kicks <- struct{}{}:
	default:
	}
}

func (m *monitor) run(ctx context.Context, server *atomic.Pointer[yamux.Session]) {
	for {
		t := time.NewTimer(m.settings.Load().IdleInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-m.kicks:
			t.Stop()
		}
		m.check(ctx, server)
	}
}

func (m *monitor) check(ctx context.Context, server *atomic.Pointer[yamux.Session]) {
	boxes, err := m.w.List()
	if err != nil {
		log.Printf("idle: list boxes: %v", err)
		return
	}
	var report api.Report
	running := map[string]bool{}
	for _, b := range boxes {
		if b.State == worker.Running && !m.w.Alive(b) {
			// Its VM died: most likely the kernel killed it when memory ran out.
			if rb, err := m.w.RollBack(b.ID); err != nil {
				log.Printf("roll back %s: %v", b.ID, err)
			} else {
				log.Printf("%s's VM died; rolled back to snapshot %d (%s)", b.ID, rb.Generation, rb.State)
				report.Boxes = append(report.Boxes, api.BoxReport{Box: toAPI(m.w, rb)})
				report.Crashed = append(report.Crashed, api.Crash{Box: b.ID, State: string(rb.State), Since: b.Checkpointed})
			}
			continue
		}
		if !m.w.Alive(b) {
			continue
		}
		running[b.ID] = true
		r, idle := m.checkBox(ctx, b)
		if idle {
			log.Printf("idle: %s inactive for %s, sleeping", b.ID, m.policy(b).Timeout)
			slept, err := m.w.Sleep(ctx, b.ID)
			if err != nil {
				// Try again after another idle timeout, not at every check.
				log.Printf("idle: sleep %s: %v", b.ID, err)
				m.seen[b.ID].lastActive = time.Now()
			} else {
				r.Box = toAPI(m.w, slept)
				r.Threads = nil
				delete(m.seen, b.ID)
				m.uploads.kick()
			}
		} else if m.dueCheckpoint(b) {
			if cp, err := m.w.Checkpoint(ctx, b.ID); err != nil {
				log.Printf("checkpoint %s: %v", b.ID, err)
			} else {
				r.Box = toAPI(m.w, cp)
				m.uploads.kick()
			}
		}
		report.Boxes = append(report.Boxes, r)
	}
	for id := range m.seen {
		if !running[id] {
			delete(m.seen, id)
		}
	}
	m.relieve(ctx, boxes, &report)
	report.Disk = diskUsage(m.w.Root)
	for _, b := range boxes {
		if b.Pending() {
			report.Uploading++
		}
	}
	if sess := server.Load(); sess != nil {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rpc.Call(rctx, sess, api.WorkerReport, report, nil); err != nil {
			log.Printf("idle: report: %v", err)
		}
		cancel()
	}
}

// dueCheckpoint reports whether a stored box has been active since its last
// checkpoint, at least checkpoint.interval ago; for a box never snapshotted,
// checkpoint.first_after, so it soon has a snapshot to recover from.
func (m *monitor) dueCheckpoint(b *worker.Box) bool {
	every := m.settings.Load().Checkpoint
	if first := m.settings.Load().FirstCheckpoint; b.Generation == 0 && first > 0 {
		every = first
	}
	a := m.seen[b.ID]
	return b.Store != nil && every > 0 && a != nil && time.Since(b.Checkpointed) >= every && a.lastActive.After(b.Checkpointed)
}

func (m *monitor) policy(b *worker.Box) worker.Idle {
	p := b.Idle
	if p.Timeout == 0 {
		p.Timeout = m.defaults.Timeout
	}
	if p.CPUThreshold == 0 {
		p.CPUThreshold = m.defaults.CPUThreshold
	}
	return p
}

func (m *monitor) checkBox(ctx context.Context, b *worker.Box) (api.BoxReport, bool) {
	now := time.Now()
	policy := m.policy(b)
	a := m.seen[b.ID]
	if a == nil {
		a = &boxActivity{lastActive: now}
		m.seen[b.ID] = a
	}
	r := api.BoxReport{Box: toAPI(m.w, b)}
	a.memMiB, r.PeakMiB = m.measure(b, a)
	r.MemMiB = a.memMiB

	if cpu, err := m.w.CPUTime(b); err == nil {
		if !a.sampled.IsZero() {
			capacity := now.Sub(a.sampled) * time.Duration(max(b.VCPUs, 1))
			if pct := 100 * float64(cpu-a.cpu) / float64(capacity); pct >= policy.CPUThreshold {
				a.lastActive = now
			}
		}
		a.cpu, a.sampled = cpu, now
	}

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := m.w.GuestStatus(sctx, b.ID)
	cancel()
	if err != nil {
		// A box we can't inspect is never put to sleep.
		a.lastActive = now
		r.LastActive = now
		return r, false
	}
	r.Files, r.Push = st.Files, st.Push
	a.attached = st.Clients > 0 || st.Sessions > 0
	if a.attached {
		a.lastActive = now
	}
	a.working = false
	window := m.settings.Load().WorkingWindow
	if a.output == nil {
		a.output = map[int]time.Time{}
	}
	for _, th := range st.Threads {
		out := time.Unix(th.LastOutput, 0)
		if prev, ok := a.output[th.N]; th.Redraw && ok {
			out = prev
		}
		a.output[th.N] = out
		// Agents print while they work (spinners, timers) and stop when they hand
		// back; a bell hands back at once.
		state := api.ThreadWaiting
		switch {
		case th.Dead:
			state = api.ThreadExited
		case !th.Bell && !th.Redraw && now.Sub(out) < window:
			state = api.ThreadWorking
			a.working = true
		}
		if out.After(a.lastActive) {
			a.lastActive = out
		}
		r.Threads = append(r.Threads, api.ThreadState{N: th.N, State: state, LastOutput: out, Viewed: th.Viewed, SessionID: th.SessionID})
	}
	r.LastActive = a.lastActive
	return r, now.Sub(a.lastActive) >= policy.Timeout
}
