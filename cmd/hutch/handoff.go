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
			if name == "" {
				if name, err = newBoxName(ctx, sess); err != nil {
					return err
				}
			}
			p := api.ForkParams{ID: name, Idle: api.Idle{Timeout: cfg.Duration("idle_timeout"), CPUThreshold: cfg.Percent("idle.cpu_threshold")},
				Credentials: cfg.List("credentials"), PushInterval: cfg.Duration("git.push_interval"), Allow: cfg.List("network.allow")}
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
					return fmt.Errorf("%s isn't in a project with a template, and this server has no base box for other directories: hutch init in the repository, then hutch template build", dir)
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
		Use:     "local <box>[/<thread>]",
		Short:   "Bring a box's work and agent sessions back here; the box sleeps",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			id, n, err := parseTarget(args[0])
			if err != nil {
				return err
			}
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
			var threads []api.Thread
			for _, t := range b.Threads {
				if (n == 0 || t.N == n) && t.SessionID != "" {
					threads = append(threads, t)
				}
			}
			if n > 0 && len(threads) == 0 {
				return fmt.Errorf("%s/%d has no agent session to bring back", id, n)
			}

			fmt.Printf("Committing and pushing %s in %s...\n", b.Branch, id)
			script := `git add -A && { git diff --cached --quiet || git commit -q -m "WIP (hutch)"; } && git push -q origin "HEAD:refs/heads/$1"`
			var stderr bytes.Buffer
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: id, Dir: "/workspace", Argv: []string{"sh", "-c", script, "sh", b.Branch}}, io.Discard, &stderr)
			if err == nil && code != 0 {
				err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
			}
			if err != nil {
				return fmt.Errorf("commit and push in %s: %w", id, err)
			}

			set, err := serverAdapters(ctx, sess)
			if err != nil {
				return err
			}
			sessions, err := boxSessions(ctx, sess, id, set, threads)
			if err != nil {
				return err
			}

			landed, why, err := bringBack(repo, b)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(b.Branch, "hutch/") {
				if err := rpc.Call(ctx, sess, api.BoxRelease, api.IDParams{ID: id}, nil); err != nil {
					return err
				}
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
				rel, data := agents.Relocate(f.transcript, f.rel, f.data, from, landed)
				dst := filepath.Join(home, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(dst, data, 0o600); err != nil {
					return err
				}
			}

			head, _ := git(landed, "log", "-1", "--format=%h %s")
			fmt.Printf("\n✓ %s at %s\n  in %s\n", b.Branch, head, landed)
			if why != "" {
				fmt.Printf("  (%s)\n", why)
			}
			if !strings.HasPrefix(b.Branch, "hutch/") {
				fmt.Printf("✓ %s released; %s is asleep and pushes no more\n", b.Branch, id)
			} else {
				fmt.Printf("✓ %s is asleep\n", id)
			}
			for _, t := range threads {
				argv, err := set.ResumeArgv(t.Agent, "ask", t.SessionID)
				if err != nil {
					continue
				}
				fmt.Printf("\nResume %s/%d (%s):\n  cd %s && %s\n", id, t.N, t.Title, landed, strings.Join(argv, " "))
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

// gitDelta fills in what a box needs to take over the current branch: HEAD, the
// commits origin lacks, staged and unstaged changes, and untracked files.
func gitDelta(root string, p *api.ForkParams) error {
	branch, _ := git(root, "symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		return errors.New("HEAD is detached; check out a branch first (the box takes over your branch)")
	}
	if def, _ := git(root, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); def == "origin/"+branch {
		return fmt.Errorf("you're on %s, the default branch; the box would push to it. Switch to a branch of your own first (git switch -c <name>)", branch)
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
	fmt.Printf("Branch:   %s @ %s (the box takes over this branch)\n", branch, commit[:7])
	delta := fmt.Sprintf("%d changed, %d untracked", nChanged, len(untracked))
	if ahead != "0" && ahead != "" {
		delta = ahead + " commits not on origin, " + delta
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

// boxSessions copies the threads' session files out of a box, with the transcript
// format of each, by the session id in their paths.
func boxSessions(ctx context.Context, sess *yamux.Session, box string, set agents.Set, threads []api.Thread) ([]sessionFile, error) {
	var out []sessionFile
	for _, t := range threads {
		a := set[t.Agent]
		if a == nil || a.Sessions == "" {
			continue
		}
		var buf, stderr bytes.Buffer
		script := `cd && find "$1" -type f -path "*$2*" -print0 | tar --null -T - -cf -`
		code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Argv: []string{"sh", "-c", script, "sh", a.SessionsRoot(), t.SessionID}}, &buf, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
		}
		if err != nil {
			return nil, fmt.Errorf("%s/%d session: %w", box, t.N, err)
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
			fmt.Printf("warning: no session files for %s/%d in the box\n", box, t.N)
		}
	}
	return out, nil
}

// bringBack puts a box's pushed branch on this laptop. A branch from hutch cloud
// is fast-forwarded in the checkout when it is on that branch, clean, and behind;
// otherwise, and for hutch/<box> branches, it goes in the box's worktree. It
// returns where the code is and, for the worktree fallback, why.
func bringBack(repo *config.Repo, b api.Box) (string, string, error) {
	if _, err := git(repo.Root, "fetch", "-q", "origin", b.Branch); err != nil {
		return "", "", err
	}
	remote := "origin/" + b.Branch
	why := ""
	if !strings.HasPrefix(b.Branch, "hutch/") {
		cur, _ := git(repo.Root, "symbolic-ref", "--short", "-q", "HEAD")
		dirty, err := git(repo.Root, "status", "--porcelain")
		if err != nil {
			return "", "", err
		}
		_, behind := git(repo.Root, "merge-base", "--is-ancestor", "HEAD", remote)
		switch {
		case cur != b.Branch:
			why = fmt.Sprintf("your checkout is on %s, not %s", cur, b.Branch)
		case dirty != "":
			why = "your checkout has local changes"
		case behind != nil:
			why = "your checkout has commits the box's branch doesn't"
		default:
			if _, err := git(repo.Root, "merge", "-q", "--ff-only", remote); err != nil {
				return "", "", err
			}
			return repo.Root, "", nil
		}
	}
	wt := worktreePath(repo, b.ID)
	if _, err := os.Stat(wt); err == nil {
		if dirty, err := git(wt, "status", "--porcelain"); err != nil {
			return "", "", err
		} else if dirty != "" {
			return "", "", fmt.Errorf("%s has local changes; commit or discard them first", wt)
		}
		if _, err := git(wt, "merge", "-q", "--ff-only", remote); err != nil {
			return "", "", err
		}
		return wt, why, nil
	}
	// A branch checked out elsewhere, or with local commits origin lacks, can't be
	// moved here without losing something, so the worktree is detached at origin's.
	list, _ := git(repo.Root, "worktree", "list", "--porcelain")
	_, ahead := git(repo.Root, "merge-base", "--is-ancestor", "refs/heads/"+b.Branch, remote)
	_, missing := git(repo.Root, "rev-parse", "-q", "--verify", "refs/heads/"+b.Branch)
	if strings.Contains(list+"\n", "branch refs/heads/"+b.Branch+"\n") || (missing == nil && ahead != nil) {
		if _, err := git(repo.Root, "worktree", "add", "-q", "--detach", wt, remote); err != nil {
			return "", "", err
		}
		if why != "" {
			why += "; "
		}
		return wt, why + "detached at origin's " + b.Branch + ", since yours can't move", nil
	}
	if _, err := git(repo.Root, "worktree", "add", "-q", "-B", b.Branch, wt, remote); err != nil {
		return "", "", err
	}
	return wt, why, nil
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
