//go:build linux

package agent

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/worker"
)

type IdleConfig struct {
	Timeout      time.Duration // sleep after this long without activity
	CPUThreshold float64       // percent of the box's vCPUs; above it counts as activity
	Interval     time.Duration // how often boxes are checked
}

// A thread counts as working while it has produced output this recently.
const workingWindow = 15 * time.Second

type boxActivity struct {
	lastActive time.Time
	cpu        time.Duration
	sampled    time.Time
}

// monitor checks running boxes, puts idle ones to sleep, and reports to the server
// when connected. It decides locally, so boxes still sleep while the server is down.
func monitor(ctx context.Context, w *worker.Worker, cfg IdleConfig, server *atomic.Pointer[yamux.Session], kick <-chan struct{}) {
	seen := map[string]*boxActivity{}
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
		boxes, err := w.List()
		if err != nil {
			log.Printf("idle: list boxes: %v", err)
			continue
		}
		var report api.Report
		running := map[string]bool{}
		for _, b := range boxes {
			if !w.Alive(b) {
				continue
			}
			running[b.ID] = true
			r, idle := check(ctx, w, b, cfg, seen)
			if idle {
				log.Printf("idle: %s inactive for %s, sleeping", b.ID, cfg.Timeout)
				slept, err := w.Sleep(ctx, b.ID)
				if err != nil {
					log.Printf("idle: sleep %s: %v", b.ID, err)
				} else {
					r.Box = toAPI(w, slept)
					r.Threads = nil
					delete(seen, b.ID)
				}
			}
			report.Boxes = append(report.Boxes, r)
		}
		for id := range seen {
			if !running[id] {
				delete(seen, id)
			}
		}
		if sess := server.Load(); sess != nil && len(report.Boxes) > 0 {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := rpc.Call(rctx, sess, api.WorkerReport, report, nil); err != nil {
				log.Printf("idle: report: %v", err)
			}
			cancel()
		}
	}
}

func check(ctx context.Context, w *worker.Worker, b *worker.Box, cfg IdleConfig, seen map[string]*boxActivity) (api.BoxReport, bool) {
	now := time.Now()
	a := seen[b.ID]
	if a == nil {
		a = &boxActivity{lastActive: now}
		seen[b.ID] = a
	}
	r := api.BoxReport{Box: toAPI(w, b)}

	if cpu, err := w.CPUTime(b); err == nil {
		if !a.sampled.IsZero() {
			capacity := now.Sub(a.sampled) * time.Duration(max(b.VCPUs, 1))
			if pct := 100 * float64(cpu-a.cpu) / float64(capacity); pct >= cfg.CPUThreshold {
				a.lastActive = now
			}
		}
		a.cpu, a.sampled = cpu, now
	}

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := w.GuestStatus(sctx, b.ID)
	cancel()
	if err != nil {
		// A box we can't inspect is never put to sleep.
		a.lastActive = now
		r.LastActive = now
		return r, false
	}
	if st.Clients > 0 || st.Sessions > 0 {
		a.lastActive = now
	}
	for _, th := range st.Threads {
		out := time.Unix(th.LastOutput, 0)
		state := api.ThreadWaiting
		switch {
		case th.Dead:
			state = api.ThreadExited
		case now.Sub(out) < workingWindow:
			state = api.ThreadWorking
		}
		if !th.Dead && out.After(a.lastActive) {
			a.lastActive = out
		}
		r.Threads = append(r.Threads, api.ThreadState{N: th.N, State: state, LastOutput: out})
	}
	r.LastActive = a.lastActive
	return r, now.Sub(a.lastActive) >= cfg.Timeout
}
