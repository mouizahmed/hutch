package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The repository lives at /workspace in every box; the recipe is .hutch/ in it.
const workspace = "/workspace"

func (s *Server) projectInit(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProjectParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	return s.store.addProject(p.Project, slugFor(p.Project))
}

func (s *Server) projectConfirm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ConfirmParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	return nil, s.store.setConfirmed(p.Project, p.Settings)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugFor names a project's boxes after the last part of its ID: github.com/me/My_App → my-app.
func slugFor(id string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(path.Base(id)), "-"), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	if slug == "" {
		slug = "project"
	}
	return slug
}

func templateBoxID(p api.Project) string { return p.Slug + "-template" }

// step prints a heading for one build step.
func step(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "\n==> "+format+"\n", args...)
}

// run runs argv in a box, streaming its output, and fails on a non-zero exit.
func (s *Server) run(ctx context.Context, w *workerConn, box string, e api.ExecParams, stdout, stderr io.Writer) error {
	e.ID = box
	code, err := rpc.Exec(ctx, w.sess, api.BoxExec, e, stdout, stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		what := e.Argv[len(e.Argv)-1]
		if len(e.Argv) > 2 && e.Argv[0] == "sh" {
			what = e.Argv[0] + " " + e.Argv[1] + " …"
		}
		return fmt.Errorf("%s exited %d", what, code)
	}
	return nil
}

func (s *Server) templateBuild(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.TemplateParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	proj, err := s.store.project(p.Project)
	if err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	return s.build(ctx, proj, p, templateBoxID(proj), stdout, stderr)
}

// build runs the recipe at p.Commit in a fresh VM, the project's template box,
// replacing any template box there was. A failed build leaves the box to inspect.
func (s *Server) build(ctx context.Context, proj api.Project, p api.TemplateParams, id string, stdout, stderr io.Writer) (api.Box, error) {
	ctx, cancel := context.WithTimeout(ctx, p.BuildTimeout)
	defer cancel()
	if _, err := s.store.box(id); err == nil {
		fmt.Fprintf(stdout, "Replacing the template box %s; changes made in it by hand are discarded.\n", id)
		if err := s.removeBox(ctx, id); err != nil {
			return api.Box{}, err
		}
	}
	if _, old := p.Recipe["setup.sh"]; old && p.Recipe["Dockerfile"] == nil {
		return api.Box{}, errors.New(".hutch/setup.sh is replaced by .hutch/Dockerfile: hutch init converts it")
	}
	b, w, err := s.bootTemplate(ctx, proj, p, id, stdout, stderr)
	if err != nil {
		return b, err
	}
	fail := func(err error) (api.Box, error) {
		return b, fmt.Errorf("%w\n  the template box is left running: hutch template shell", err)
	}

	step(stdout, "checkout %.12s", p.Commit)
	if err := s.run(ctx, w, id, api.ExecParams{Dir: workspace, Argv: []string{"git", "checkout", "-q", "--detach", p.Commit}}, stdout, stderr); err != nil {
		return fail(fmt.Errorf("commit %.12s is not on the remote (push it first): %w", p.Commit, err))
	}

	secrets, digest, err := s.projectSecretFiles(proj.ID)
	if err != nil {
		return b, err
	}
	if len(secrets) > 0 {
		step(stdout, "project secrets (%d files)", len(secrets))
		if err := s.writeSecrets(ctx, w, id, secrets); err != nil {
			return fail(err)
		}
	}

	sampleCtx, stopSampling := context.WithCancel(ctx)
	peaks := make(chan api.Peaks, 1)
	go func() { peaks <- s.samplePeaks(sampleCtx, w, id) }()
	defer stopSampling()

	if err := s.installAgents(ctx, w, id, stdout, stderr); err != nil {
		return fail(err)
	}
	if _, ok := p.Recipe["services"]; ok {
		step(stdout, "services")
		if err := s.run(ctx, w, id, api.ExecParams{User: "root", Argv: []string{"sh", "-c",
			"install -m644 " + workspace + "/.hutch/services " + guestproto.ServicesPath + " && kill -HUP 1"}}, stdout, stderr); err != nil {
			return fail(err)
		}
	}
	if _, ok := p.Recipe["on-create.sh"]; ok {
		step(stdout, "on-create.sh (as dev)")
		if err := s.run(ctx, w, id, api.ExecParams{Dir: workspace, Argv: []string{"bash", "-eo", "pipefail", ".hutch/on-create.sh"}}, stdout, stderr); err != nil {
			return fail(err)
		}
	}

	_, verify := p.Recipe["verify.sh"]
	if verify {
		step(stdout, "verify.sh (as dev)")
		if err := s.run(ctx, w, id, api.ExecParams{Dir: workspace, Argv: []string{"bash", "-eo", "pipefail", ".hutch/verify.sh"}}, stdout, stderr); err != nil {
			return fail(err)
		}
	}
	stopSampling()
	pk := <-peaks
	pk.OfVCPUs, pk.OfMiB, pk.At = p.VCPUs, p.MemMiB, time.Now().UTC()
	s.store.setPeaks(proj.ID, pk)
	fmt.Fprintf(stdout, "\nPeak use: %.1f GB memory, %.1f vCPUs (of %d vCPU, %.0f GB)\n", float64(pk.MemMiB)/1024, pk.CPUs, p.VCPUs, float64(p.MemMiB)/1024)

	// Whatever verify.sh left running would be running in every box; the services
	// (each in its own session, started by init) stay.
	s.run(ctx, w, id, api.ExecParams{User: "root", Argv: []string{"sh", "-c", stopLeftovers}}, io.Discard, io.Discard)

	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate, commit: p.Commit, verified: true, secrets: digest}); err != nil {
		return b, err
	}
	if verify {
		fmt.Fprintf(stdout, "\nBuilt and verified %.12s in %s.\n", p.Commit, id)
	} else {
		fmt.Fprintf(stdout, "\nBuilt %.12s in %s. There is no .hutch/verify.sh, so nothing checked that the project builds or runs here.\n", p.Commit, id)
	}
	return b, nil
}

// stopLeftovers kills dev's processes outside the sessions of init's services.
const stopLeftovers = `keep=" $(sed 's/},{/}\n{/g' ` + guestproto.InitStatusPath + ` 2>/dev/null | grep '"uid":1000' | sed 's/.*"pid":\([0-9]*\).*/\1/' | tr '\n' ' ') "
for p in $(pgrep -u dev); do s=$(ps -o sid= -p $p | tr -d ' '); case "$keep" in *" $s "*) ;; *) kill -KILL $p 2>/dev/null;; esac; done; true`

// bootTemplate cold boots the project's template box from its image (built first
// when the recipe changed) with the repository cloned at /workspace.
func (s *Server) bootTemplate(ctx context.Context, proj api.Project, p api.TemplateParams, id string, stdout, stderr io.Writer) (api.Box, *workerConn, error) {
	n := need{memMiB: p.MemMiB, vcpus: p.VCPUs}
	if err := s.checkFits(ctx, n); err != nil {
		return api.Box{}, nil, err
	}
	w, release, err := s.placeWait(ctx, n, p.Worker, id, stderr)
	if err != nil {
		return api.Box{}, nil, err
	}
	defer release()
	img, from, err := s.buildImage(ctx, w, proj, p, stdout)
	if err != nil {
		return api.Box{}, nil, err
	}
	step(stdout, "boot %s (%d vCPU, %d MiB)", id, p.VCPUs, p.MemMiB)
	b, err := s.callForBox(ctx, w, api.BoxBoot, api.BootParams{ID: id, Image: img, VCPUs: p.VCPUs, MemMiB: p.MemMiB, DiskBytes: p.DiskBytes, Idle: p.Idle, Allow: p.Allow})
	if err != nil {
		return b, nil, err
	}
	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate}); err != nil {
		return b, nil, err
	}
	if err := s.store.setImage(id, from); err != nil {
		return b, nil, err
	}
	if err := s.ensureTools(ctx, w, id, stdout, stderr); err != nil {
		return b, w, err
	}
	if err := s.applyUserLayer(ctx, w, id, p.Credentials); err != nil {
		return b, w, err
	}
	step(stdout, "clone %s", p.CloneURL)
	var msg bytes.Buffer
	if err := s.run(ctx, w, id, api.ExecParams{Argv: []string{"git", "clone", "-q", p.CloneURL, workspace}}, stdout, io.MultiWriter(stderr, &msg)); err != nil {
		if strings.Contains(msg.String(), "could not read Username") {
			err = fmt.Errorf("%w; a private repository needs a GitHub login: hutch login gh", err)
		}
		return b, w, fmt.Errorf("clone: %w", err)
	}
	return b, w, nil
}

// buildImage builds the template's image on w: the recipe's Dockerfile, the
// published image the project names, or neither, the default image. An image is
// named after what it's built from, so an unchanged recipe reuses it.
func (s *Server) buildImage(ctx context.Context, w *workerConn, proj api.Project, p api.TemplateParams, out io.Writer) (name, from string, err error) {
	cfg, err := s.settings()
	if err != nil {
		return "", "", err
	}
	bp := api.ImageBuildParams{Name: api.DefaultImage, CacheBytes: cfg.Size("machine.build_cache"), Reuse: true, Pins: p.Pins}
	switch {
	case p.Image != "":
		bp.Name, bp.Ref = fmt.Sprintf("%s-%x", proj.Slug, sha256.Sum256([]byte(p.Image)))[:len(proj.Slug)+13], p.Image
		step(out, "image %s", p.Image)
	case p.Recipe["Dockerfile"] != nil:
		h := sha256.New()
		names := make([]string, 0, len(p.Recipe))
		for n := range p.Recipe {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(h, "%s\x00%d\x00", n, len(p.Recipe[n]))
			h.Write(p.Recipe[n])
		}
		bp.Name, bp.Files = fmt.Sprintf("%s-%x", proj.Slug, h.Sum(nil))[:len(proj.Slug)+13], p.Recipe
		step(out, "image from .hutch/Dockerfile")
	}
	ctx, cancel := context.WithTimeout(ctx, p.BuildTimeout)
	defer cancel()
	var img api.Image
	if err := rpc.CallOutput(ctx, w.sess, api.ImageBuild, bp, &img, out, out); err != nil {
		return "", "", fmt.Errorf("image: %w", err)
	}
	from = strings.Join(img.Sources, ", ")
	if from != "" && bp.Name != api.DefaultImage {
		fmt.Fprintf(out, "%s, from %s\n", img.Name, from)
	}
	return img.Name, from, nil
}

func (s *Server) templateSnapshot(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.TemplateParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	proj, err := s.store.project(p.Project)
	if err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	id := templateBoxID(proj)
	m, err := s.store.meta(id)
	if err != nil {
		return nil, err
	}
	b, _ := s.store.box(id)
	digest, _, err := s.store.projectSecrets(proj.ID)
	if err != nil {
		return nil, err
	}
	if m.verified && m.commit == p.Commit && m.secrets == digest && b.State == "running" {
		fmt.Fprintf(stdout, "Using the build of %.12s in %s.\n", p.Commit, id)
	} else if _, err := s.build(ctx, proj, p, id, stdout, stderr); err != nil {
		return nil, err
	}
	w, err := s.workerFor(id)
	if err != nil {
		return nil, err
	}

	step(stdout, "wait for services")
	if err := s.waitQuiet(ctx, w, id, p.QuietTimeout); err != nil {
		return nil, err
	}
	s.run(ctx, w, id, api.ExecParams{User: "root", Argv: []string{"sh", "-c", "sync; echo 3 > /proc/sys/vm/drop_caches"}}, io.Discard, io.Discard)

	step(stdout, "snapshot")
	if _, err := s.callForBox(ctx, w, api.BoxSleep, api.IDParams{ID: id}); err != nil {
		return nil, err
	}
	versions, err := s.store.templates(proj.ID)
	if err != nil {
		return nil, err
	}
	n := 1
	if len(versions) > 0 {
		n = versions[0].Version + 1
	}
	vid := fmt.Sprintf("%s-v%d", proj.Slug, n)
	// Read before the box's row goes: its meta goes with it.
	built, err := s.store.meta(id)
	if err != nil {
		return nil, err
	}
	s.hostProject(ctx, w, proj.ID)
	vb, err := s.callForBox(ctx, w, api.BoxRename, api.RenameParams{ID: id, NewID: vid, Store: s.versionStore(proj.ID, w.info.Class, n)})
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteBox(id); err != nil {
		return nil, err
	}
	if err := s.store.putMeta(vid, boxMeta{project: proj.ID, kind: api.BoxVersion, commit: p.Commit, secrets: built.secrets}); err != nil {
		return nil, err
	}
	t := api.Template{Project: proj.ID, Version: n, Commit: p.Commit, Box: vid, VCPUs: vb.VCPUs, MemMiB: vb.MemMiB, Created: time.Now().UTC(), Secrets: built.secrets, Image: built.image}
	if err := s.store.addTemplate(t); err != nil {
		return nil, err
	}
	if params, err := json.Marshal(p); err == nil {
		s.store.setTemplateParams(proj.ID, n, string(params))
	}
	fmt.Fprintf(stdout, "\nTemplate v%d = recipe @ %.12s\n", n, p.Commit)
	s.pruneVersions(ctx, proj.ID)
	return t, nil
}

// waitQuiet waits until every service init runs has been up a few seconds: the
// snapshot has them running, not starting.
func (s *Server) waitQuiet(ctx context.Context, w *workerConn, box string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var out bytes.Buffer
		if err := s.run(ctx, w, box, api.ExecParams{User: "root", Argv: []string{"cat", guestproto.InitStatusPath}}, &out, io.Discard); err != nil {
			return err
		}
		var st []guestproto.ServiceStatus
		if err := json.Unmarshal(out.Bytes(), &st); err != nil {
			return fmt.Errorf("init status: %w", err)
		}
		var down []string
		for _, sv := range st {
			if sv.PID == 0 || time.Since(sv.Since) < 3*time.Second {
				down = append(down, fmt.Sprintf("%s (restarted %d times; see /var/log/hutch)", sv.Name, sv.Fails))
			}
		}
		if len(down) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("services not up after %s; every box would start with them down:\n  %s", timeout, strings.Join(down, "\n  "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) templateList(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProjectParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	return s.store.templates(p.Project)
}

func (s *Server) projectList(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	ids, err := s.store.projects()
	if err != nil {
		return nil, err
	}
	out := []api.Project{}
	for _, id := range ids {
		p, err := s.store.project(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// templateBox returns the project's template box, starting one when there is none:
// a fork of the newest version, or before the first version a fresh VM with the
// repository cloned.
func (s *Server) templateBox(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.TemplateParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	proj, err := s.store.project(p.Project)
	if err != nil {
		return nil, err
	}
	id := templateBoxID(proj)
	if b, err := s.store.box(id); err == nil {
		return b, nil
	}
	versions, err := s.store.templates(proj.ID)
	if err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	if len(versions) == 0 {
		b, w, err := s.bootTemplate(ctx, proj, p, id, stdout, stderr)
		if err != nil {
			return b, err
		}
		b.Branch, b.Base, err = s.checkoutBranch(ctx, w, id, "hutch/template", "", stderr)
		return b, err
	}
	v := versions[0]
	w, release, source, err := s.versionWorker(ctx, v, id, stderr)
	if err != nil {
		return nil, err
	}
	defer release()
	b, err := s.callForBox(ctx, w, api.BoxFork, api.ForkParams{Source: source, ID: id, Idle: p.Idle, Allow: p.Allow})
	if err != nil {
		return nil, err
	}
	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate, commit: v.Commit, version: v.Version}); err != nil {
		return nil, err
	}
	if err := s.applyUserLayer(ctx, w, id, p.Credentials); err != nil {
		return nil, err
	}
	b.Branch, b.Base, err = s.checkoutBranch(ctx, w, id, "hutch/template", "", stderr)
	return b, err
}

// forkProject creates a box from the project's newest template version and checks
// out its branch. A failing on-create.sh is reported, not fatal: the box is usable.
func (s *Server) forkProject(ctx context.Context, p api.ForkParams, stdout, stderr io.Writer) (api.Box, error) {
	versions, err := s.store.templates(p.Project)
	if err != nil {
		return api.Box{}, err
	}
	if len(versions) == 0 {
		return api.Box{}, errors.New("this project has no template yet: hutch template snapshot (or hutch new --from base)")
	}
	v := versions[0]
	w, release, source, err := s.versionWorker(ctx, v, p.ID, stderr)
	if err != nil {
		return api.Box{}, err
	}
	defer release()
	s.hostProject(ctx, w, p.Project)
	p.Source, p.Store = source, s.boxStore(p.Project, p.ID)
	b, err := s.callForBox(ctx, w, api.BoxFork, p)
	if err != nil {
		return b, err
	}
	if err := s.store.putMeta(b.ID, boxMeta{project: p.Project, commit: v.Commit, version: v.Version}); err != nil {
		return b, err
	}
	if err := s.linkPath(ctx, w, b.ID, p.Path, stderr); err != nil {
		return b, err
	}
	if err := s.applyUserLayer(ctx, w, b.ID, p.Credentials); err != nil {
		return b, err
	}
	if p.Branch != "" {
		step(stdout, "your changes")
		if b.Branch, b.Base, err = s.takeOverBranch(ctx, w, b.ID, p, stderr); err != nil {
			s.removeBox(ctx, b.ID)
			return api.Box{}, err
		}
	} else {
		b.Branch, b.Base, err = s.checkoutBranch(ctx, w, b.ID, "hutch/"+b.ID, p.Base, stderr)
	}
	if err != nil {
		return b, err
	}
	if s.run(ctx, w, b.ID, api.ExecParams{Dir: workspace, Argv: []string{"test", "-f", ".hutch/on-create.sh"}}, io.Discard, io.Discard) == nil {
		step(stdout, "on-create.sh")
		if err := s.run(ctx, w, b.ID, api.ExecParams{Dir: workspace, Argv: []string{"bash", "-eo", "pipefail", ".hutch/on-create.sh"}}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "warning: on-create.sh failed (%v); the box is created anyway\n", err)
		}
	}
	return b, nil
}

// writeSecrets puts project secrets into the checkout and keeps Git from ever
// committing them, so an agent's "git add -A" can't push them.
func (s *Server) writeSecrets(ctx context.Context, w *workerConn, box string, files []guestproto.File) error {
	var exclude []string
	out := make([]guestproto.File, len(files))
	for i, f := range files {
		out[i] = guestproto.File{Path: workspace + "/" + f.Path, Mode: 0o600, Data: f.Data}
		exclude = append(exclude, "/"+f.Path)
	}
	if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{Files: out}}, nil); err != nil {
		return err
	}
	return s.run(ctx, w, box, api.ExecParams{Dir: workspace, Argv: append([]string{"sh", "-c",
		`for p; do grep -qxF "$p" .git/info/exclude 2>/dev/null || echo "$p" >> .git/info/exclude; done`, "sh"}, exclude...)}, io.Discard, io.Discard)
}

// checkoutBranch puts a box's /workspace on its own branch, continuing it if it is
// already on origin, else starting it from base (empty: the default branch). It
// returns the branch and base.
func (s *Server) checkoutBranch(ctx context.Context, w *workerConn, box, branch, base string, stderr io.Writer) (string, string, error) {
	var out bytes.Buffer
	if err := s.run(ctx, w, box, api.ExecParams{Dir: workspace, Argv: []string{"sh", "-ec", `git fetch -q origin
base="${2:-$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's,^origin/,,')}"
from="origin/$base"
git rev-parse -q --verify "refs/remotes/origin/$1" >/dev/null && from="origin/$1"
git checkout -q --no-track -B "$1" "$from"
echo "$base"`, "sh", branch, base}}, &out, stderr); err != nil {
		return "", "", fmt.Errorf("checkout: %w", err)
	}
	base = strings.TrimSpace(out.String())
	return branch, base, s.store.setBoxGit(box, branch, base)
}

// removeBox deletes a box on its worker and in the bucket, and forgets it. A box
// on a lost worker is removed from the bucket alone; the worker drops its copy if
// it ever comes back.
func (s *Server) removeBox(ctx context.Context, id string) error {
	b, err := s.store.box(id)
	if err != nil {
		return err
	}
	store, _ := s.storeOf(b)
	if b.Kind == api.BoxVersion {
		holders, _ := s.store.versionWorkers(id)
		for _, wid := range holders {
			if w, err := s.worker(wid); err == nil && wid != b.Worker {
				if err := rpc.Call(ctx, w.sess, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
					return err
				}
				s.store.deleteVersionCopy(id, wid)
			}
		}
	}
	if w, err := s.worker(b.Worker); err == nil {
		if err := rpc.Call(ctx, w.sess, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
			return err
		}
	} else if lost, lerr := s.workerLost(b.Worker); lerr != nil || !lost {
		return fmt.Errorf("%s is on a machine that is offline; remove it once that machine is back", id)
	}
	if store != nil {
		s.deletePrefix(ctx, store.Prefix)
	}
	s.store.event(id, api.BoxRm, "removed")
	s.mu.Lock()
	delete(s.threadStates, id)
	s.mu.Unlock()
	return s.store.deleteBox(id)
}

// samplePeaks reads the guest's memory and CPU use every two seconds until ctx ends.
func (s *Server) samplePeaks(ctx context.Context, w *workerConn, box string) api.Peaks {
	var peak api.Peaks
	var lastBusy, lastTotal, cores float64
	for {
		var out bytes.Buffer
		err := s.run(ctx, w, box, api.ExecParams{Argv: []string{"sh", "-c", "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; head -1 /proc/stat; nproc"}}, &out, io.Discard)
		if err == nil {
			var total, avail int
			for _, line := range strings.Split(out.String(), "\n") {
				f := strings.Fields(line)
				switch {
				case len(f) >= 2 && f[0] == "MemTotal:":
					total, _ = strconv.Atoi(f[1])
				case len(f) >= 2 && f[0] == "MemAvailable:":
					avail, _ = strconv.Atoi(f[1])
				case len(f) > 5 && f[0] == "cpu":
					var sum float64
					for _, v := range f[1:] {
						n, _ := strconv.ParseFloat(v, 64)
						sum += n
					}
					idle, _ := strconv.ParseFloat(f[4], 64)
					iowait, _ := strconv.ParseFloat(f[5], 64)
					busy := sum - idle - iowait
					if lastTotal > 0 && sum > lastTotal && cores > 0 {
						peak.CPUs = max(peak.CPUs, cores*(busy-lastBusy)/(sum-lastTotal))
					}
					lastBusy, lastTotal = busy, sum
				case len(f) == 1:
					cores, _ = strconv.ParseFloat(f[0], 64)
				}
			}
			peak.MemMiB = max(peak.MemMiB, (total-avail)/1024)
		}
		select {
		case <-ctx.Done():
			return peak
		case <-time.After(2 * time.Second):
		}
	}
}

// pruneVersions removes a project's old template versions beyond the newest
// retention.template_versions, keeping any a box was forked from. Forks share
// nothing with a version that removing it would break (their files are reflinks
// and hard links), but moves and handoffs later build on it.
func (s *Server) pruneVersions(ctx context.Context, project string) {
	cfg, err := s.settings()
	if err != nil {
		log.Printf("prune %s: %v", project, err)
		return
	}
	versions, err := s.store.templates(project)
	if err != nil {
		log.Printf("prune %s: %v", project, err)
		return
	}
	inUse, commits, err := s.store.versionsInUse(project)
	if err != nil {
		log.Printf("prune %s: %v", project, err)
		return
	}
	keep := int(cfg.Int("retention.template_versions"))
	for i, v := range versions {
		if i < keep || inUse[v.Version] || commits[v.Commit] {
			continue
		}
		if err := s.removeBox(ctx, v.Box); err != nil {
			log.Printf("prune %s v%d: %v", project, v.Version, err)
			continue
		}
		builds, _ := s.store.templateBuilds(project, v.Version)
		for _, b := range builds {
			if b.box != v.Box {
				s.removeBox(ctx, b.box)
			}
		}
		s.store.deleteTemplate(project, v.Version)
		log.Printf("pruned template %s v%d", project, v.Version)
	}
}

// pruneAll applies template retention to every project, as workers connect.
func (s *Server) pruneAll(ctx context.Context) {
	ids, err := s.store.projects()
	if err != nil {
		return
	}
	for _, id := range ids {
		s.pruneVersions(ctx, id)
	}
}

// projectRm removes a project's template box and versions and forgets it. Boxes
// must be removed first: they hold work.
func (s *Server) projectRm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProjectParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	proj, err := s.store.project(p.Project)
	if err != nil {
		return nil, err
	}
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	var owned, work []string
	for _, b := range boxes {
		if b.Project != proj.ID {
			continue
		}
		if b.Kind == "" {
			work = append(work, b.ID)
		}
		owned = append(owned, b.ID)
	}
	if len(work) > 0 {
		return nil, fmt.Errorf("remove its boxes first: %s", strings.Join(work, ", "))
	}
	for _, id := range owned {
		if err := s.removeBox(ctx, id); err != nil {
			return nil, err
		}
	}
	s.deletePrefix(ctx, s.projectPrefix(proj.ID))
	return nil, s.store.deleteProject(proj.ID)
}
