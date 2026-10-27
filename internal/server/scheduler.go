package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sort"
	"strings"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// need is what a box needs from a worker.
type need struct {
	memMiB, vcpus int
	// class is the snapshot class its memory was taken on; with strict (a fork) only
	// that class will do, otherwise another class means a cold boot.
	class  string
	strict bool
	// version is the template version box it forks from: a worker that has it
	// starts the box without downloading it.
	version string
	// avoid is a worker it mustn't go to: the one it moves off.
	avoid string
}

func (n need) String() string { return fmt.Sprintf("%d vCPU, %s", n.vcpus, mib(n.memMiB)) }

func mib(m int) string {
	if m%1024 == 0 {
		return fmt.Sprintf("%d GB", m/1024)
	}
	return fmt.Sprintf("%d MB", m)
}

type candidate struct {
	id         string
	freeMiB    int // memory not reserved by awake boxes, after the host's margin
	cpus       int
	diskOK     bool     // free disk above worker.disk_reserve
	classes    []string // snapshot classes it restores warm
	elastic    bool
	hasVersion bool
}

// why says why a box doesn't fit a worker, or "" when it does.
func (c candidate) why(n need) string {
	switch {
	case c.freeMiB < n.memMiB:
		return fmt.Sprintf("%s free", mib(max(c.freeMiB, 0)))
	case c.cpus < n.vcpus:
		return fmt.Sprintf("%d vCPUs", c.cpus)
	case !c.diskOK:
		return "disk below worker.disk_reserve"
	case n.strict && n.class != "" && !slices.Contains(c.classes, n.class):
		return "snapshot class " + strings.Join(c.classes, ", ")
	}
	return ""
}

// choose picks a worker for a box: of those it fits, persistent over elastic, its
// snapshot class over another, one that has its template, then the fullest, so
// lightly used workers empty out and can be stopped. It returns -1 if none fits.
func choose(cs []candidate, n need) int {
	best := -1
	for i, c := range cs {
		if c.why(n) == "" && (best < 0 || better(c, cs[best], n)) {
			best = i
		}
	}
	return best
}

func better(a, b candidate, n need) bool {
	if a.elastic != b.elastic {
		return !a.elastic
	}
	if am, bm := slices.Contains(a.classes, n.class), slices.Contains(b.classes, n.class); am != bm {
		return am
	}
	if a.hasVersion != b.hasVersion {
		return a.hasVersion
	}
	if a.freeMiB != b.freeMiB {
		return a.freeMiB < b.freeMiB
	}
	return a.id < b.id
}

// place picks a worker for a box (requested, if set) and holds the box's memory
// there until release, so boxes placed at the same time don't all land on the one
// worker that had room for one of them.
func (s *Server) place(n need, requested string) (*workerConn, func(), error) {
	cfg, err := s.settings()
	if err != nil {
		return nil, nil, err
	}
	margin, reserve := cfg.Percent("worker.memory_margin")/100, cfg.Percent("worker.disk_reserve")/100
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, nil, err
	}
	reserved := map[string]int{}
	for _, b := range boxes {
		if b.State == "running" {
			reserved[b.Worker] += b.MemMiB
		}
	}
	ws, err := s.store.workers()
	if err != nil {
		return nil, nil, err
	}
	draining, elastic := map[string]bool{}, map[string]bool{}
	for _, w := range ws {
		draining[w.ID], elastic[w.ID] = w.Draining, w.Node != ""
	}
	copies := map[string]bool{}
	if n.version != "" {
		ws, err := s.store.versionWorkers(n.version)
		if err != nil {
			return nil, nil, err
		}
		for _, w := range ws {
			copies[w] = true
		}
	}

	running := 0
	for _, b := range boxes {
		if b.State == "running" {
			running++
		}
	}
	awakeLimit := s.limitInt("limits.awake_boxes")

	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(running+s.starting) >= awakeLimit {
		return nil, nil, &limitError{"limits.awake_boxes", awakeLimit}
	}
	var cs []candidate
	var conns []*workerConn
	for id, w := range s.workers {
		if (requested != "" && id != requested) || id == n.avoid || (requested == "" && draining[id]) {
			continue
		}
		d := w.info.Disk
		cs = append(cs, candidate{
			id:         id,
			freeMiB:    int(float64(w.info.MemMiB)*(1-margin)) - reserved[id] - s.pending[id],
			cpus:       w.info.CPUs,
			diskOK:     d.Total == 0 || float64(d.Free) >= reserve*float64(d.Total),
			classes:    workerClasses(w.info),
			elastic:    elastic[id],
			hasVersion: copies[id],
		})
		conns = append(conns, w)
	}
	if requested != "" && len(cs) == 0 {
		return nil, nil, fmt.Errorf("worker %s is offline", requested)
	}
	i := choose(cs, n)
	if i < 0 {
		if len(cs) == 0 {
			return nil, nil, fmt.Errorf("%w: no workers online", errNoRoom)
		}
		var reasons []string
		for i, c := range cs {
			reasons = append(reasons, fmt.Sprintf("%s: %s", conns[i].info.Name, c.why(n)))
		}
		sort.Strings(reasons)
		return nil, nil, fmt.Errorf("%w for a %s box (%s)", errNoRoom, n, strings.Join(reasons, "; "))
	}
	w := conns[i]
	s.pending[w.id] += n.memMiB
	s.starting++
	return w, func() {
		s.mu.Lock()
		s.pending[w.id] -= n.memMiB
		s.starting--
		s.mu.Unlock()
	}, nil
}

// checkFits refuses a box size no worker could ever hold, so it fails at once
// instead of waiting for room that never comes. The candidates are the persistent
// workers and, with a provider, providers.aws.worker_type.
func (s *Server) checkFits(ctx context.Context, n need) error {
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	margin := cfg.Percent("worker.memory_margin") / 100
	ws, err := s.store.workers()
	if err != nil {
		return err
	}
	var candidates []string
	known := false
	fits := func(name string, cpus, memMiB int) bool {
		known = true
		usable := int(float64(memMiB) * (1 - margin))
		candidates = append(candidates, fmt.Sprintf("%s: %d vCPUs, %s for boxes", name, cpus, mib(usable)))
		return usable >= n.memMiB && cpus >= n.vcpus
	}
	for _, w := range ws {
		if w.Node == "" && !w.Gone && w.MemMiB > 0 && fits(w.Name, w.CPUs, w.MemMiB) {
			return nil
		}
	}
	if f := s.fleet.Load(); f != nil {
		wtype := cfg.String("providers.aws.worker_type")
		if t, err := f.instanceType(ctx, wtype); err == nil && fits(wtype+" (elastic)", t.VCPUs, t.MemMiB) {
			return nil
		}
	}
	if !known {
		return nil // nothing to judge by until a worker connects
	}
	return fmt.Errorf("a %s box fits no worker (%s); choose a smaller size, or a larger providers.aws.worker_type",
		n, strings.Join(candidates, "; "))
}

// versionWorker returns a worker with a copy of a template version that can take
// a fork of it, copying the version there from the bucket when the chosen worker
// doesn't have it. release frees the fork's reservation.
func (s *Server) versionWorker(ctx context.Context, v api.Template, box string, out io.Writer) (*workerConn, func(), error) {
	src, err := s.store.box(v.Box)
	if err != nil {
		return nil, nil, err
	}
	n := need{memMiB: v.MemMiB, vcpus: v.VCPUs, class: src.Class, strict: true, version: v.Box}
	if err := s.checkFits(ctx, n); err != nil {
		return nil, nil, err
	}
	w, release, err := s.placeWait(ctx, n, "", box, out)
	if err != nil {
		return nil, nil, err
	}
	// Boxes forked at once onto a new worker copy the template there once.
	mu, _ := s.copying.LoadOrStore(v.Box+"@"+w.id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	have, err := s.store.versionWorkers(v.Box)
	if err != nil {
		release()
		return nil, nil, err
	}
	for _, id := range have {
		if id == w.id {
			return w, release, nil
		}
	}
	store, err := s.storeOf(src)
	if err == nil {
		var gen int
		if gen, err = s.latestGeneration(ctx, store); err == nil {
			s.hostProject(ctx, w, v.Project)
			err = rpc.Call(ctx, w.sess, api.BoxRestore, api.RestoreParams{ID: v.Box, Store: *store, Generation: gen}, nil)
			if err != nil && strings.Contains(err.Error(), "already exists") {
				err = nil // the worker has it; the server hadn't recorded it
			}
		}
	}
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("copy template v%d to %s: %w", v.Version, w.info.Name, err)
	}
	s.store.addVersionCopy(v.Box, w.id)
	s.store.event(v.Box, "copied", "to "+w.id)
	return w, release, nil
}

// workerClasses are the snapshot classes a worker restores warm (just its one
// class, for a hutchd from before side-by-side Firecracker versions).
func workerClasses(w api.Worker) []string {
	if len(w.Classes) > 0 {
		return w.Classes
	}
	return []string{w.Class}
}

// boxPlan says what forking the project's newest template would do: which worker
// and why, without starting anything.
func (s *Server) boxPlan(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ForkParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	versions, err := s.store.templates(p.Project)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("this project has no template yet: hutch template snapshot")
	}
	v := versions[0]
	src, err := s.store.box(v.Box)
	if err != nil {
		return nil, err
	}
	n := need{memMiB: v.MemMiB, vcpus: v.VCPUs, class: src.Class, strict: true, version: v.Box}
	plan := api.Plan{Template: fmt.Sprintf("v%d (recipe @ %.12s)", v.Version, v.Commit), Size: n.String(), Class: src.Class}
	if err := s.checkFits(ctx, n); err != nil {
		return nil, err
	}
	plan.Warnings = s.fitWarnings(ctx, n)
	if lim, err := s.spendReached(); err != nil {
		return nil, err
	} else if lim != nil {
		plan.Why = "queued: " + lim.Error()
		return plan, nil
	}
	w, release, err := s.place(n, "")
	if err == nil {
		release()
		plan.Worker, plan.Kind = w.info.Name, "persistent"
		have, _ := s.store.versionWorkers(v.Box)
		var why string
		if slices.Contains(have, w.id) {
			why = "has the template and room"
		} else {
			why = "has room; the template is copied there from the bucket first"
		}
		ws, _ := s.store.workers()
		for _, x := range ws {
			if x.ID == w.id && x.Node != "" {
				plan.Kind = "elastic"
				plan.PerHour = s.perHour(ctx, []api.Box{{ID: "plan", Worker: w.id, State: "running", MemMiB: n.memMiB}})["plan"]
			}
		}
		plan.Why = why
		return plan, nil
	}
	var lim *limitError
	switch {
	case errors.As(err, &lim):
		plan.Why = "queued: " + lim.Error()
	case errors.Is(err, errNoRoom) && s.fleet.Load() != nil:
		f := s.fleet.Load()
		cfg, _ := s.settings()
		wtype := cfg.String("providers.aws.worker_type")
		plan.Kind = "elastic"
		if s.cloudRunning(ctx, f) >= int(s.limitInt("limits.cloud_workers")) {
			plan.Why = "queued: limits.cloud_workers reached"
		} else if t, terr := f.instanceType(ctx, wtype); terr == nil {
			plan.Why = fmt.Sprintf("no worker has room: a stopped %s worker is started, or a new one (%d vCPU, %s, $%.3f/h)", wtype, t.VCPUs, mib(t.MemMiB), t.PerHour)
		}
	case errors.Is(err, errNoRoom):
		plan.Why = "queued until a worker has room: " + err.Error()
	default:
		return nil, err
	}
	return plan, nil
}

// fitWarnings warns when a box size fits only one box per worker.
func (s *Server) fitWarnings(ctx context.Context, n need) []string {
	cfg, err := s.settings()
	if err != nil {
		return nil
	}
	margin := cfg.Percent("worker.memory_margin") / 100
	var out []string
	check := func(name string, memMiB int) {
		if per := int(float64(memMiB)*(1-margin)) / n.memMiB; per == 1 {
			out = append(out, fmt.Sprintf("only one %s box fits on %s", n, name))
		}
	}
	ws, _ := s.store.workers()
	for _, w := range ws {
		if w.Node == "" && !w.Gone && w.MemMiB > 0 {
			check(w.Name, w.MemMiB)
		}
	}
	if f := s.fleet.Load(); f != nil {
		wtype := cfg.String("providers.aws.worker_type")
		if t, err := f.instanceType(ctx, wtype); err == nil {
			check(wtype, t.MemMiB)
		}
	}
	return out
}
