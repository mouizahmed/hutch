package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

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
	diskOK     bool     // free disk above machine.disk_reserve
	classes    []string // snapshot classes it restores warm
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
		return "disk below machine.disk_reserve"
	case n.strict && n.class != "" && !slices.Contains(c.classes, n.class):
		return "snapshot class " + strings.Join(c.classes, ", ")
	}
	return ""
}

// choose picks a worker for a box: of those it fits, its snapshot class over
// another, one that has its template, then the fullest, so lightly used workers
// empty out. It returns -1 if none fits.
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
	margin, reserve := cfg.Percent("machine.memory_margin")/100, cfg.Percent("machine.disk_reserve")/100
	overcommit, high := cfg.Float("machine.overcommit"), cfg.Percent("machine.memory_high")
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, nil, err
	}
	// reserved is what awake boxes take: their full sizes, or with overcommit their
	// measured peaks (full size until measured); promised is always full sizes.
	reserved, promised := map[string]int{}, map[string]int{}
	s.mu.Lock()
	for _, b := range boxes {
		if b.State != "running" {
			continue
		}
		promised[b.Worker] += b.MemMiB
		if p := s.peaks[b.ID]; overcommit > 1 && p > 0 {
			reserved[b.Worker] += min(p, b.MemMiB)
		} else {
			reserved[b.Worker] += b.MemMiB
		}
	}
	s.mu.Unlock()
	ws, err := s.store.workers()
	if err != nil {
		return nil, nil, err
	}
	draining := map[string]bool{}
	for _, w := range ws {
		draining[w.ID] = w.Draining
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
		free := int(float64(w.info.MemMiB)*(1-margin)) - reserved[id]
		if overcommit > 1 {
			free = min(free, int(float64(w.info.MemMiB)*overcommit)-promised[id])
			// A worker under memory pressure takes nothing new.
			if w.memTotal > 0 && 100*float64(w.memTotal-w.memAvailable)/float64(w.memTotal) >= high {
				free = 0
			}
		}
		cs = append(cs, candidate{
			id:         id,
			freeMiB:    free - s.pending[id],
			cpus:       w.info.CPUs,
			diskOK:     d.Total == 0 || float64(d.Free) >= reserve*float64(d.Total),
			classes:    workerClasses(w.info),
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
			return nil, nil, fmt.Errorf("%w: hutchd's worker isn't connected", errNoRoom)
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
// instead of waiting for room that never comes.
func (s *Server) checkFits(ctx context.Context, n need) error {
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	margin := cfg.Percent("machine.memory_margin") / 100
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
		if !w.Gone && w.MemMiB > 0 && fits(w.Name, w.CPUs, w.MemMiB) {
			return nil
		}
	}
	if !known {
		return nil // nothing to judge by until a worker connects
	}
	return fmt.Errorf("a %s box doesn't fit the machine (%s); choose a smaller size, or use a bigger machine",
		n, strings.Join(candidates, "; "))
}

// versionWorker returns a worker that can take a fork of template version v, and
// the build to fork from. It prefers a worker of a snapshot class the version has
// a build for, copying that build there from the bucket if the worker doesn't have
// it. When no worker runs any of those classes, it builds the version again for
// the class of a worker with room: the same recipe commit,
// since snapshots don't cross classes. release frees the fork's reservation.
func (s *Server) versionWorker(ctx context.Context, v api.Template, box string, out io.Writer) (*workerConn, func(), string, error) {
	n := need{memMiB: v.MemMiB, vcpus: v.VCPUs}
	if err := s.checkFits(ctx, n); err != nil {
		return nil, nil, "", err
	}
	for attempt := 0; attempt < 2; attempt++ {
		builds, err := s.store.templateBuilds(v.Project, v.Version)
		if err != nil {
			return nil, nil, "", err
		}
		for _, b := range builds {
			bn := n
			bn.class, bn.strict, bn.version = b.class, true, b.box
			if w, release, err := s.place(bn, ""); err == nil {
				if err := s.copyVersion(ctx, w, v, b.box); err != nil {
					release()
					return nil, nil, "", err
				}
				return w, release, b.box, nil
			}
		}
		w, release, err := s.placeWait(ctx, n, "", box, out)
		if err != nil {
			return nil, nil, "", err
		}
		release()
		// Room may have freed on a worker that runs one of the version's classes.
		have := false
		for _, b := range builds {
			have = have || slices.Contains(workerClasses(w.info), b.class)
		}
		if have {
			continue
		}
		if _, err := s.buildForClass(ctx, v, w, out); err != nil {
			return nil, nil, "", err
		}
	}
	return nil, nil, "", fmt.Errorf("no worker can run template v%d", v.Version)
}

// copyVersion makes sure a worker has a template build, copying it from the bucket.
func (s *Server) copyVersion(ctx context.Context, w *workerConn, v api.Template, buildBox string) error {
	// Boxes forked at once onto a new worker copy the build there once.
	mu, _ := s.copying.LoadOrStore(buildBox+"@"+w.id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	have, err := s.store.versionWorkers(buildBox)
	if err != nil {
		return err
	}
	if slices.Contains(have, w.id) {
		return nil
	}
	src, err := s.store.box(buildBox)
	if err != nil {
		return err
	}
	store, err := s.storeOf(src)
	if err == nil {
		var gen int
		if gen, err = s.latestGeneration(ctx, store); err == nil {
			s.hostProject(ctx, w, v.Project)
			err = rpc.Call(ctx, w.sess, api.BoxRestore, api.RestoreParams{ID: buildBox, Store: *store, Generation: gen}, nil)
			if err != nil && strings.Contains(err.Error(), "already exists") {
				err = nil // the worker has it; the server hadn't recorded it
			}
		}
	}
	if err != nil {
		return fmt.Errorf("copy template v%d to %s: %w", v.Version, w.info.Name, err)
	}
	s.store.addVersionCopy(buildBox, w.id)
	s.store.event(buildBox, "copied", "to "+w.id)
	return nil
}

// buildForClass builds template version v again on worker w, for its snapshot
// class, from the recipe and settings v was built with.
func (s *Server) buildForClass(ctx context.Context, v api.Template, w *workerConn, out io.Writer) (string, error) {
	proj, err := s.store.project(v.Project)
	if err != nil {
		return "", err
	}
	raw, err := s.store.templateParams(v.Project, v.Version)
	if err != nil || raw == "" {
		return "", fmt.Errorf("template v%d can't be built for snapshot class %s: it predates per-class builds; run hutch template snapshot", v.Version, w.info.Class)
	}
	var p api.TemplateParams
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "", err
	}
	p.Worker = w.id
	class := w.info.Class
	tag := fmt.Sprintf("%x", sha256.Sum256([]byte(class)))[:6]
	id, buildID := fmt.Sprintf("%s-v%d-%s", proj.Slug, v.Version, tag), fmt.Sprintf("%s-v%d-%s-build", proj.Slug, v.Version, tag)
	fmt.Fprintf(out, "Template v%d has no snapshot for %s yet; building one there (a few minutes, once).\n", v.Version, class)
	if _, err := s.store.box(buildID); err == nil {
		s.removeBox(ctx, buildID)
	}
	if _, err := s.build(ctx, proj, p, buildID, out, out); err != nil {
		return "", err
	}
	if err := s.waitQuiet(ctx, w, buildID, p.QuietTimeout); err != nil {
		return "", err
	}
	s.run(ctx, w, buildID, api.ExecParams{User: "root", Argv: []string{"sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches"}}, io.Discard, io.Discard)
	if _, err := s.callForBox(ctx, w, api.BoxSleep, api.IDParams{ID: buildID}); err != nil {
		return "", err
	}
	s.hostProject(ctx, w, proj.ID)
	if _, err := s.callForBox(ctx, w, api.BoxRename, api.RenameParams{ID: buildID, NewID: id, Store: s.versionStore(proj.ID, class, v.Version)}); err != nil {
		return "", err
	}
	built, _ := s.store.meta(buildID)
	s.store.deleteBox(buildID)
	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxVersion, commit: v.Commit, secrets: built.secrets}); err != nil {
		return "", err
	}
	if err := s.store.addTemplateBuild(proj.ID, v.Version, class, id); err != nil {
		return "", err
	}
	s.store.event(id, "built", fmt.Sprintf("template v%d for %s", v.Version, class))
	return id, nil
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
	w, release, err := s.place(n, "")
	if err == nil {
		release()
		plan.Worker = w.info.Name
		have, _ := s.store.versionWorkers(v.Box)
		var why string
		if slices.Contains(have, w.id) {
			why = "has the template and room"
		} else {
			why = "has room; the template is copied there from the bucket first"
		}
		plan.Why = why
		return plan, nil
	}
	var lim *limitError
	switch {
	case errors.As(err, &lim):
		plan.Why = "queued: " + lim.Error()
	case errors.Is(err, errNoRoom):
		plan.Why = "queued until there is room: " + err.Error()
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
	margin := cfg.Percent("machine.memory_margin") / 100
	var out []string
	check := func(name string, memMiB int) {
		if per := int(float64(memMiB)*(1-margin)) / n.memMiB; per == 1 {
			out = append(out, fmt.Sprintf("only one %s box fits on %s", n, name))
		}
	}
	ws, _ := s.store.workers()
	for _, w := range ws {
		if !w.Gone && w.MemMiB > 0 {
			check(w.Name, w.MemMiB)
		}
	}
	return out
}

// workerClasses are the snapshot classes a worker restores warm (just its one
// class, for a hutchd from before side-by-side Firecracker versions).
func workerClasses(w api.Worker) []string {
	if len(w.Classes) > 0 {
		return w.Classes
	}
	return []string{w.Class}
}
