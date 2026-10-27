//go:build linux

package agent

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/worker"
)

type memSample struct {
	at  time.Time
	mib int
}

// measure records a running box's memory and returns it with its peak over the window.
func (m *monitor) measure(b *worker.Box, a *boxActivity) (now, peak int) {
	mib, err := m.w.MemUse(b)
	if err != nil {
		return 0, 0
	}
	t := time.Now()
	a.samples = append(a.samples, memSample{t, mib})
	window := m.settings.Load().PeakWindow
	if window == 0 {
		window = time.Hour
	}
	keep := a.samples[:0]
	for _, s := range a.samples {
		if t.Sub(s.at) <= window {
			keep = append(keep, s)
			peak = max(peak, s.mib)
		}
	}
	a.samples = keep
	return mib, peak
}

// relieve handles memory pressure under overcommit, mildest step first: take back
// memory guests aren't using, sleep idle boxes (they lose nothing), ask the server
// to move a working box, and past the critical mark, sleep the least recently
// active working box. It runs on the worker, so it works while the server is down.
func (m *monitor) relieve(ctx context.Context, boxes []*worker.Box, report *api.Report) {
	total, avail := worker.HostMemory()
	report.MemTotal, report.MemAvailable = total, avail
	s := m.settings.Load()
	if s.Overcommit <= 1 || total == 0 {
		return
	}
	used := func() float64 {
		t, a := worker.HostMemory()
		return 100 * float64(t-a) / float64(t)
	}
	var running []*worker.Box
	for _, b := range boxes {
		if m.w.Alive(b) && m.seen[b.ID] != nil {
			running = append(running, b)
		}
	}
	pct := used()
	if pct < s.MemoryHigh {
		if pct < s.MemoryHigh-10 {
			for _, b := range running {
				if a := m.seen[b.ID]; a.reclaimed {
					if err := m.w.Release(ctx, b); err == nil {
						a.reclaimed = false
					}
				}
			}
		}
		return
	}
	log.Printf("memory: %.0f%% used (high at %.0f%%)", pct, s.MemoryHigh)
	for _, b := range running {
		a := m.seen[b.ID]
		if n, err := m.w.Reclaim(ctx, b, 256); err == nil && n > 0 {
			a.reclaimed = true
			log.Printf("memory: reclaimed %d MiB from %s", n, b.ID)
		}
	}
	if pct = used(); pct < s.MemoryHigh {
		return
	}
	sort.Slice(running, func(i, j int) bool { return m.seen[running[i].ID].memMiB > m.seen[running[j].ID].memMiB })
	sleep := func(b *worker.Box, why string) bool {
		slept, err := m.w.Sleep(ctx, b.ID)
		if err != nil {
			log.Printf("memory: sleep %s: %v", b.ID, err)
			return false
		}
		log.Printf("memory: slept %s (%s)", b.ID, why)
		for i := range report.Boxes {
			if report.Boxes[i].Box.ID == b.ID {
				report.Boxes[i].Box, report.Boxes[i].Threads = toAPI(m.w, slept), nil
			}
		}
		delete(m.seen, b.ID)
		m.uploads.kick()
		return true
	}
	var working []*worker.Box
	for _, b := range running {
		a := m.seen[b.ID]
		// Recent CPU use counts too: a silent build is working.
		if a.working || a.attached || time.Since(a.lastActive) < s.WorkingWindow {
			working = append(working, b)
			continue
		}
		if sleep(b, "idle, under memory pressure") {
			if pct = used(); pct < s.MemoryHigh {
				return
			}
		}
	}
	report.Pressure = "high"
	if pct < s.MemoryCritical || len(working) == 0 {
		return
	}
	sort.Slice(working, func(i, j int) bool { return m.seen[working[i].ID].lastActive.Before(m.seen[working[j].ID].lastActive) })
	if sleep(working[0], "least recently active, memory critical") {
		report.ForceSlept = append(report.ForceSlept, working[0].ID)
	}
}
