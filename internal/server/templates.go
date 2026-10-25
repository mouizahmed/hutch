package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Every template build starts from this worker image.
const baseImage = "base"

// The repository lives at /workspace in every box; the recipe is .hutch/ in it.
const workspace = "/workspace"

func (s *Server) projectInit(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProjectParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	return s.store.addProject(p.Project, slugFor(p.Project))
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
		return fmt.Errorf("%s exited %d", e.Argv[len(e.Argv)-1], code)
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
	return s.build(ctx, proj, p, stdout, stderr)
}

// build runs the recipe at p.Commit in a fresh VM, the project's template box,
// replacing any template box there was. A failed build leaves the box to inspect.
func (s *Server) build(ctx context.Context, proj api.Project, p api.TemplateParams, stdout, stderr io.Writer) (api.Box, error) {
	ctx, cancel := context.WithTimeout(ctx, p.BuildTimeout)
	defer cancel()
	id := templateBoxID(proj)
	if _, err := s.store.box(id); err == nil {
		fmt.Fprintf(stdout, "Replacing the template box %s; changes made in it by hand are discarded.\n", id)
		if err := s.removeBox(ctx, id); err != nil {
			return api.Box{}, err
		}
	}
	b, w, err := s.bootTemplate(ctx, proj, p, stdout, stderr)
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
	if err := s.run(ctx, w, id, api.ExecParams{Dir: workspace, Argv: []string{"test", "-f", ".hutch/setup.sh", "-a", "-f", ".hutch/verify.sh"}}, io.Discard, io.Discard); err != nil {
		return fail(errors.New("the recipe needs .hutch/setup.sh and .hutch/verify.sh at that commit (hutch init writes a prompt to draft them)"))
	}

	step(stdout, "setup.sh (as root)")
	if err := s.run(ctx, w, id, api.ExecParams{User: "root", Dir: workspace, Env: []string{"DEBIAN_FRONTEND=noninteractive"},
		Argv: []string{"bash", "-eo", "pipefail", ".hutch/setup.sh"}}, stdout, stderr); err != nil {
		return fail(err)
	}

	if err := s.installAgents(ctx, w, id, stdout, stderr); err != nil {
		return fail(err)
	}

	step(stdout, "verify.sh (as dev)")
	if err := s.run(ctx, w, id, api.ExecParams{Dir: workspace, Argv: []string{"bash", "-eo", "pipefail", ".hutch/verify.sh"}}, stdout, stderr); err != nil {
		return fail(err)
	}
	// Whatever verify.sh left running would be running in every box.
	s.run(ctx, w, id, api.ExecParams{User: "root", Argv: []string{"pkill", "-KILL", "-u", "dev"}}, io.Discard, io.Discard)

	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate, commit: p.Commit, verified: true}); err != nil {
		return b, err
	}
	fmt.Fprintf(stdout, "\nBuilt and verified %.12s in %s.\n", p.Commit, id)
	return b, nil
}

// bootTemplate cold boots the project's template box from the base image with the
// repository cloned at /workspace.
func (s *Server) bootTemplate(ctx context.Context, proj api.Project, p api.TemplateParams, stdout, stderr io.Writer) (api.Box, *workerConn, error) {
	id := templateBoxID(proj)
	w, err := s.placement("")
	if err != nil {
		return api.Box{}, nil, err
	}
	step(stdout, "boot %s (%d vCPU, %d MiB)", id, p.VCPUs, p.MemMiB)
	b, err := s.callForBox(ctx, w, api.BoxBoot, api.BootParams{ID: id, Image: baseImage, VCPUs: p.VCPUs, MemMiB: p.MemMiB, DiskBytes: p.DiskBytes, Idle: p.Idle, Allow: p.Allow})
	if err != nil {
		return b, nil, err
	}
	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate}); err != nil {
		return b, nil, err
	}
	if err := s.applyUserLayer(ctx, w, id, p.Credentials); err != nil {
		return b, w, err
	}
	step(stdout, "clone %s", p.CloneURL)
	if err := s.run(ctx, w, id, api.ExecParams{Argv: []string{"git", "clone", "-q", p.CloneURL, workspace}}, stdout, stderr); err != nil {
		return b, w, fmt.Errorf("clone: %w", err)
	}
	return b, w, nil
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
	if m.verified && m.commit == p.Commit && b.State == "running" {
		fmt.Fprintf(stdout, "Using the build of %.12s in %s.\n", p.Commit, id)
	} else if _, err := s.build(ctx, proj, p, stdout, stderr); err != nil {
		return nil, err
	}
	w, err := s.workerFor(id)
	if err != nil {
		return nil, err
	}

	step(stdout, "wait for systemd to be quiet")
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
	vb, err := s.callForBox(ctx, w, api.BoxRename, api.RenameParams{ID: id, NewID: vid})
	if err != nil {
		return nil, err
	}
	if err := s.store.deleteBox(id); err != nil {
		return nil, err
	}
	if err := s.store.putMeta(vid, boxMeta{project: proj.ID, kind: api.BoxVersion, commit: p.Commit}); err != nil {
		return nil, err
	}
	t := api.Template{Project: proj.ID, Version: n, Commit: p.Commit, Box: vid, VCPUs: vb.VCPUs, MemMiB: vb.MemMiB, Created: time.Now().UTC()}
	if err := s.store.addTemplate(t); err != nil {
		return nil, err
	}
	fmt.Fprintf(stdout, "\nTemplate v%d = recipe @ %.12s\n", n, p.Commit)
	return t, nil
}

// waitQuiet waits until systemd has no queued jobs and no transient units: anything
// pending in the snapshot would run again in every box restored from it.
func (s *Server) waitQuiet(ctx context.Context, w *workerConn, box string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var out bytes.Buffer
		err := s.run(ctx, w, box, api.ExecParams{User: "root", Argv: []string{"sh", "-c",
			"systemctl list-jobs --no-legend --no-pager; ls -A /run/systemd/transient 2>/dev/null || true"}}, &out, io.Discard)
		if err != nil {
			return err
		}
		pending := strings.TrimSpace(out.String())
		if pending == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd still has queued jobs or transient units after %s; they would run in every box:\n%s", timeout, pending)
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
		b, w, err := s.bootTemplate(ctx, proj, p, stdout, stderr)
		if err != nil {
			return b, err
		}
		b.Branch, b.Base, err = s.checkoutBranch(ctx, w, id, "hutch/template", "", p.PushInterval, stderr)
		return b, err
	}
	v := versions[0]
	src, err := s.store.box(v.Box)
	if err != nil {
		return nil, err
	}
	w, err := s.worker(src.Worker)
	if err != nil {
		return nil, err
	}
	b, err := s.callForBox(ctx, w, api.BoxFork, api.ForkParams{Source: v.Box, ID: id, Idle: p.Idle, Allow: p.Allow})
	if err != nil {
		return nil, err
	}
	if err := s.store.putMeta(id, boxMeta{project: proj.ID, kind: api.BoxTemplate, commit: v.Commit}); err != nil {
		return nil, err
	}
	if err := s.applyUserLayer(ctx, w, id, p.Credentials); err != nil {
		return nil, err
	}
	b.Branch, b.Base, err = s.checkoutBranch(ctx, w, id, "hutch/template", "", p.PushInterval, stderr)
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
	src, err := s.store.box(v.Box)
	if err != nil {
		return api.Box{}, err
	}
	w, err := s.worker(src.Worker)
	if err != nil {
		return api.Box{}, err
	}
	p.Source = v.Box
	b, err := s.callForBox(ctx, w, api.BoxFork, p)
	if err != nil {
		return b, err
	}
	if err := s.store.putMeta(b.ID, boxMeta{project: p.Project, commit: v.Commit}); err != nil {
		return b, err
	}
	if err := s.applyUserLayer(ctx, w, b.ID, p.Credentials); err != nil {
		return b, err
	}
	if b.Branch, b.Base, err = s.checkoutBranch(ctx, w, b.ID, "hutch/"+b.ID, p.Base, p.PushInterval, stderr); err != nil {
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

// checkoutBranch puts a box's /workspace on its own branch, continuing it if it is
// already on origin, else starting it from base (empty: the default branch), and
// sets the guest to push it after commits. It returns the branch and base.
func (s *Server) checkoutBranch(ctx context.Context, w *workerConn, box, branch, base string, interval time.Duration, stderr io.Writer) (string, string, error) {
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
	if err := s.store.setBoxGit(box, branch, base); err != nil {
		return "", "", err
	}
	conf, _ := json.Marshal(guestproto.PushConfig{Dir: workspace, Branch: branch, Interval: interval})
	if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{User: "root",
		Files: []guestproto.File{{Path: guestproto.PushConfigPath, Mode: 0o644, Data: conf}}}}, nil); err != nil {
		return "", "", fmt.Errorf("push config: %w", err)
	}
	return branch, base, nil
}

// removeBox deletes a box on its worker and forgets it.
func (s *Server) removeBox(ctx context.Context, id string) error {
	w, err := s.workerFor(id)
	if err != nil {
		return err
	}
	if err := rpc.Call(ctx, w.sess, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
		return err
	}
	s.store.event(id, api.BoxRm, "removed")
	return s.store.deleteBox(id)
}
