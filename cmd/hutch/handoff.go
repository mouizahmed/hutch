package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/names"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// What hutch cloud carries in one call to the server, which takes up to 64 MB.
const maxHandoff = 32 << 20

func handoffCommands() []*cobra.Command {
	var name, agentName string
	var noSession bool
	cloud := &cobra.Command{
		Use:     "cloud [<dir>]",
		Short:   "Move this work and its agent session into a new box",
		GroupID: "boxes",
		Args:    cobra.MaximumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			// Agents record the resolved directory they ran in.
			dir, err := filepath.Abs(dir)
			if err == nil {
				dir, err = filepath.EvalSymlinks(dir)
			}
			if err == nil {
				err = os.Chdir(dir)
			}
			if err != nil {
				return err
			}
			cfg, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			if cfg, err = applyGrants(ctx, sess, cfg); err != nil {
				return err
			}
			ensureGitIdentity(ctx, sess)
			if name == "" {
				if name, err = newBoxName(ctx, sess); err != nil {
					return err
				}
			}
			p := api.ForkParams{ID: name, Idle: api.Idle{Timeout: cfg.Duration("idle_timeout"), CPUThreshold: cfg.Percent("idle.cpu_threshold")},
				Credentials: cfg.List("credentials"), Allow: cfg.List("network.allow")}
			if err := checkAllow(p.Allow); err != nil {
				return err
			}
			repo, err := config.FindRepo(dir)
			if err != nil {
				return err
			}
			project := false
			if repo != nil {
				var versions []api.Template
				if err := rpc.Call(ctx, sess, api.TemplateList, api.ProjectParams{Project: repo.ID}, &versions); err != nil {
					return err
				}
				project = len(versions) > 0
			}
			if project {
				p.Project, p.Path = repo.ID, repo.Root
				if err := gitDelta(repo.Root, &p); err != nil {
					return err
				}
			} else {
				if _, err := findBox(ctx, sess, "base"); err != nil {
					return fmt.Errorf("%s isn't in a project with a template, and the machine has no base box for other directories: hutch init in the repository, then hutch template build", dir)
				}
				root := dir
				if repo != nil {
					root = repo.Root
				}
				files, err := dirFiles(root)
				if err != nil {
					return err
				}
				data, err := tarFiles(root, files)
				if err != nil {
					return err
				}
				p.Source, p.Path, p.Delta = "base", root, &api.Delta{Files: data}
				fmt.Printf("Files:    %d from %s (%s; no project template, so a base box)\n", len(files), root, size(len(data)))
			}
			if p.Delta.Size() > maxHandoff {
				return fmt.Errorf("the changes come to %s; hutch cloud carries up to %s: commit and push large files first, or ignore them", size(p.Delta.Size()), size(maxHandoff))
			}

			set, err := serverAdapters(ctx, sess)
			if err != nil {
				return err
			}
			home, _ := os.UserHomeDir()
			var found *agents.Session
			for _, a := range set.Sorted() {
				if noSession || !a.Agent() || a.Resume == "" || (agentName != "" && a.Name != agentName) {
					continue
				}
				s, err := agents.LatestSession(a, home, dir)
				if err != nil {
					return err
				}
				if s != nil && (found == nil || s.Modified.After(found.Modified)) {
					found = s
				}
			}
			tp := api.ThreadParams{Agent: agentName, Autonomy: cfg.String("autonomy"), Dir: dir}
			if tp.Agent == "" {
				tp.Agent = cfg.String("default_agent")
			}
			if found != nil {
				tp.Agent, tp.Resume = found.Agent, found.ID
				total := 0
				for _, rel := range found.Files {
					data, err := os.ReadFile(filepath.Join(home, rel))
					if err != nil {
						return err
					}
					total += len(data)
					tp.Files = append(tp.Files, guestproto.File{Path: "~/" + filepath.ToSlash(rel), Mode: 0o600, Data: data})
				}
				if total > maxHandoff {
					return fmt.Errorf("the %s session is %s; hutch cloud carries up to %s", found.Agent, size(total), size(maxHandoff))
				}
				fmt.Printf("Session:  %s session %s, last written %s\n", found.Agent, found.ID, ago(found.Modified))
				if time.Since(found.Modified) < 30*time.Second {
					fmt.Printf("          If %s is still running here, quit it: what it does from now on stays on this laptop.\n", found.Agent)
				}
			} else if !noSession {
				fmt.Printf("Session:  none found for %s; starting a new %s\n", dir, tp.Agent)
			}

			start := time.Now()
			var b api.Box
			if err := rpc.CallOutput(ctx, sess, api.BoxFork, p, &b, os.Stdout, os.Stderr); err != nil {
				return err
			}
			tp.Box = b.ID
			var t api.Thread
			if err := rpc.Call(ctx, sess, api.ThreadNew, tp, &t); err != nil {
				return fmt.Errorf("box %s has your work, but the thread failed: %w", b.ID, err)
			}
			verb := "started"
			if tp.Resume != "" {
				verb = "resumed your session"
			}
			fmt.Printf("\n✓ %s/%d %s %s in %s\n  hutch attach %s/%d\n", b.ID, t.N, t.Agent, verb, time.Since(start).Round(100*time.Millisecond), b.ID, t.N)
			if project {
				if dirty, _ := git(repo.Root, "status", "--porcelain"); dirty != "" {
					msg := "hutch cloud " + b.ID
					if _, err := git(repo.Root, "stash", "push", "-q", "-u", "-m", msg); err != nil {
						fmt.Printf("Couldn't stash your local changes (%v); the box has them too.\n", err)
					} else {
						fmt.Printf("Stashed your local changes as %q; the box has them, so drop it once you're done.\n", msg)
					}
				}
			}
			fmt.Println("You can close your laptop.")
			return nil
		}),
	}
	cloud.Flags().StringVar(&name, "name", "", "box name (default: generated)")
	cloud.Flags().StringVar(&agentName, "agent", "", "only hand over this agent's session")
	cloud.Flags().BoolVar(&noSession, "no-session", false, "move only the code; start a new agent")
	cloud.MarkFlagsMutuallyExclusive("agent", "no-session")

	local := &cobra.Command{
		Use:     "local <box>",
		Short:   "Bring a box's branch and agent sessions into this checkout; the box sleeps",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			id := args[0]
			b, err := boxBranch(ctx, sess, id)
			if err != nil {
				return err
			}
			repo, err := config.FindRepo(".")
			if err != nil {
				return err
			}
			if repo == nil || repo.ID != b.Project {
				return fmt.Errorf("run this in a checkout of %s", b.Project)
			}
			// Every session of the box's agents comes back; the agent's own picker
			// chooses among them.
			var agentNames []string
			for _, t := range b.Threads {
				if t.Agent != "" && !slices.Contains(agentNames, t.Agent) {
					agentNames = append(agentNames, t.Agent)
				}
			}

			if dirty, err := git(repo.Root, "status", "--porcelain"); err != nil {
				return err
			} else if dirty != "" {
				return fmt.Errorf("%s has uncommitted changes; commit or stash them first, so the box's branch can come in", repo.Root)
			}

			branch, head, wip, err := commitBox(ctx, sess, id)
			if err != nil {
				return err
			}
			if wip {
				fmt.Printf("Committed %s's uncommitted work as \"WIP (hutch)\".\n", id)
			}
			if err := fetchBox(ctx, sess, id, repo.Root, "refs/heads/"+branch, head); err != nil {
				return err
			}

			set, err := serverAdapters(ctx, sess)
			if err != nil {
				return err
			}
			sessions, err := boxSessions(ctx, sess, id, set, agentNames)
			if err != nil {
				return err
			}
			if err := checkoutHere(repo.Root, branch, head); err != nil {
				return err
			}
			var slept api.Box
			if err := rpc.Call(ctx, sess, api.BoxSleep, api.IDParams{ID: id}, &slept); err != nil {
				fmt.Printf("warning: %s didn't sleep: %v\n", id, err)
			}

			from := b.Path
			if from == "" {
				from = "/workspace"
			}
			home, _ := os.UserHomeDir()
			for _, f := range sessions {
				rel, data := agents.Relocate(f.transcript, f.rel, f.data, from, repo.Root)
				dst := filepath.Join(home, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(dst, data, 0o600); err != nil {
					return err
				}
			}

			subject, _ := git(repo.Root, "log", "-1", "--format=%h %s")
			fmt.Printf("\n✓ %s at %s\n  in %s\n✓ %s is asleep\n", branch, subject, repo.Root, id)
			for _, name := range agentNames {
				argv, err := set.PickArgv(name, "ask")
				if err != nil {
					continue
				}
				fmt.Printf("\nContinue a %s session here:\n  %s\n", name, strings.Join(argv, " "))
			}
			return nil
		}),
	}
	return []*cobra.Command{cloud, local}
}

func newBoxName(ctx context.Context, sess *yamux.Session) (string, error) {
	boxes, err := listBoxes(ctx, sess)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, b := range boxes {
		taken[b.ID] = true
	}
	return names.New(taken), nil
}

// serverAdapters is the adapter set the server uses: built-ins and the user's own.
func serverAdapters(ctx context.Context, sess *yamux.Session) (agents.Set, error) {
	var st api.UserState
	if err := rpc.Call(ctx, sess, api.UserGet, struct{}{}, &st); err != nil {
		return nil, err
	}
	return agents.Load(st.Layer.Adapters)
}

// gitDelta fills in what a box needs to continue the current branch: HEAD, the
// commits origin lacks, staged and unstaged changes, and untracked files.
func gitDelta(root string, p *api.ForkParams) error {
	branch, _ := git(root, "symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		return errors.New("HEAD is detached; check out a branch first (the box continues your branch)")
	}
	commit, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	d := &api.Delta{}
	ahead, _ := git(root, "rev-list", "--count", "HEAD", "--not", "--remotes=origin")
	if ahead != "0" && ahead != "" {
		f, err := os.CreateTemp("", "hutch-bundle-*")
		if err != nil {
			return err
		}
		f.Close()
		defer os.Remove(f.Name())
		if _, err := git(root, "bundle", "create", "-q", f.Name(), "HEAD", "--not", "--remotes=origin"); err != nil {
			return err
		}
		if d.Bundle, err = os.ReadFile(f.Name()); err != nil {
			return err
		}
	}
	if d.Staged, err = gitRaw(root, "diff", "--cached", "--binary"); err != nil {
		return err
	}
	if d.Unstaged, err = gitRaw(root, "diff", "--binary"); err != nil {
		return err
	}
	list, err := gitRaw(root, "ls-files", "-o", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	untracked := splitNul(list)
	if d.Files, err = tarFiles(root, untracked); err != nil {
		return err
	}
	if len(untracked) == 0 {
		d.Files = nil
	}
	changed, _ := git(root, "diff", "--name-only", "HEAD")
	nChanged := 0
	if changed != "" {
		nChanged = len(strings.Split(changed, "\n"))
	}
	p.Branch, p.Commit, p.Delta = branch, commit, d
	if def, _ := git(root, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); def == "origin/"+branch {
		fmt.Printf("Branch:   %s @ %s (the default branch, so the box works on hutch/%s from here)\n", branch, commit[:7], p.ID)
	} else {
		fmt.Printf("Branch:   %s @ %s (the box continues this branch)\n", branch, commit[:7])
	}
	delta := fmt.Sprintf("%d changed, %d untracked", nChanged, len(untracked))
	if ahead != "0" && ahead != "" {
		delta = plural(ahead, "commit") + " not on origin, " + delta
	}
	fmt.Printf("Delta:    %s (%s)\n", delta, size(d.Size()))
	return nil
}

func gitRaw(root string, args ...string) ([]byte, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

func splitNul(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// dirFiles lists what hutch cloud copies from a directory outside a project: in a
// Git repository, the files Git doesn't ignore and the repository itself; else all.
func dirFiles(root string) ([]string, error) {
	var files []string
	walk := func(dir string) error {
		return filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files = append(files, rel)
			return nil
		})
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return files, walk(".")
	}
	list, err := gitRaw(root, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	files = splitNul(list)
	return files, walk(".git")
}

// tarFiles archives files and symlinks under root; anything else is skipped.
func tarFiles(root string, files []string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, rel := range files {
		p := filepath.Join(root, rel)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue // listed by Git but deleted
		}
		if err != nil {
			return nil, err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return nil, err
			}
		} else if !fi.Mode().IsRegular() {
			continue
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return nil, err
		}
		h.Name, h.Uname, h.Gname, h.Uid, h.Gid = filepath.ToSlash(rel), "", "", 0, 0
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type sessionFile struct {
	transcript, rel string
	data            []byte
}

// boxSessions copies the agents' session files out of a box, with the transcript
// format of each: everything under each agent's sessions directory, since a box
// works on one checkout.
func boxSessions(ctx context.Context, sess *yamux.Session, box string, set agents.Set, names []string) ([]sessionFile, error) {
	var out []sessionFile
	for _, name := range names {
		a := set[name]
		if a == nil || a.Sessions == "" {
			continue
		}
		var buf, stderr bytes.Buffer
		script := `cd && [ -d "$1" ] || exit 0; find "$1" -type f -print0 | tar --null -T - -cf -`
		code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Argv: []string{"sh", "-c", script, "sh", a.SessionsRoot()}}, &buf, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
		}
		if err != nil {
			return nil, fmt.Errorf("%s's %s sessions: %w", box, name, err)
		}
		tr := tar.NewReader(&buf)
		found := false
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag != tar.TypeReg || strings.Contains(h.Name, "..") {
				continue
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			out = append(out, sessionFile{transcript: a.Transcript, rel: h.Name, data: data})
			found = true
		}
		if !found {
			fmt.Printf("warning: no %s session files in %s\n", name, box)
		}
	}
	return out, nil
}

// commitBox commits a box's uncommitted work as WIP, the one commit Hutch makes,
// and returns its branch and head.
func commitBox(ctx context.Context, sess *yamux.Session, box string) (branch, head string, wip bool, err error) {
	// A box made before it had a git identity commits as this computer's.
	script := `set -e
branch=$(git symbolic-ref --short -q HEAD) || { echo "HEAD is detached in the box; check out a branch first" >&2; exit 3; }
wip=no
git add -A
if ! git diff --cached --quiet; then
  if git config user.email >/dev/null; then git commit -q -m "WIP (hutch)"
  else git -c "user.name=$1" -c "user.email=$2" commit -q -m "WIP (hutch)"; fi
  wip=yes
fi
echo "$branch $(git rev-parse HEAD) $wip"`
	var out, stderr bytes.Buffer
	code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Dir: "/workspace", Argv: []string{"sh", "-c", script, "sh", gitGlobal("user.name"), gitGlobal("user.email")}}, &out, &stderr)
	if err == nil && code != 0 {
		err = errors.New(strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", box, err)
	}
	f := strings.Fields(out.String())
	if len(f) != 3 {
		return "", "", false, fmt.Errorf("%s: unexpected answer %q", box, out.String())
	}
	return f[0], f[1], f[2] == "yes", nil
}

// fetchBox brings a box's commits up to head, at ref there, into the repository at
// root as a git bundle over Hutch's connection, so no remote is needed: everything
// since the commits this repository's branches already have.
func fetchBox(ctx context.Context, sess *yamux.Session, box, root, ref, head string) error {
	if _, err := git(root, "cat-file", "-e", head+"^{commit}"); err == nil {
		return nil
	}
	tips, err := git(root, "for-each-ref", "--format=%(objectname)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return err
	}
	script := `not=
for c; do git cat-file -e "$c^{commit}" 2>/dev/null && not="$not ^$c"; done
git bundle create -q - "$0" $not`
	f, err := os.CreateTemp("", "hutch-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	var stderr bytes.Buffer
	code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Dir: "/workspace", Argv: append([]string{"sh", "-c", script, ref}, strings.Fields(tips)...)}, f, &stderr)
	f.Close()
	if err == nil && code != 0 {
		err = errors.New(strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return fmt.Errorf("bundle %s's commits: %w", box, err)
	}
	if _, err := git(root, "fetch", "-q", f.Name(), ref); err != nil {
		return fmt.Errorf("take in %s's commits: %w", box, err)
	}
	return nil
}

// checkoutHere checks out branch at head in the clean checkout at root: a new
// branch, or a fast-forward of the one here. One that has diverged is left alone.
func checkoutHere(root, branch, head string) error {
	if _, err := git(root, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err != nil {
		_, err := git(root, "checkout", "-q", "--no-track", "-b", branch, head)
		return err
	}
	if _, err := git(root, "merge-base", "--is-ancestor", "refs/heads/"+branch, head); err != nil {
		return fmt.Errorf("your %s has commits the box's doesn't; nothing was changed here. Merge the box's work with: git switch %s && git merge %s", branch, branch, head[:12])
	}
	if _, err := git(root, "checkout", "-q", branch); err != nil {
		return err
	}
	_, err := git(root, "merge", "-q", "--ff-only", head)
	return err
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n, word string) string {
	switch {
	case n == "1":
		return n + " " + word
	case strings.HasSuffix(word, "x"):
		return n + " " + word + "es"
	}
	return n + " " + word + "s"
}
