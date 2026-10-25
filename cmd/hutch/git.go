package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// findBox returns a box by name, with its threads, branch, and base.
func findBox(ctx context.Context, sess *yamux.Session, id string) (api.Box, error) {
	boxes, err := listBoxes(ctx, sess)
	if err != nil {
		return api.Box{}, err
	}
	for _, b := range boxes {
		if b.ID == id {
			return b, nil
		}
	}
	return api.Box{}, fmt.Errorf("no box %q", id)
}

// boxBranch returns a box with a branch Hutch manages.
func boxBranch(ctx context.Context, sess *yamux.Session, id string) (api.Box, error) {
	b, err := findBox(ctx, sess, id)
	if err == nil && b.Branch == "" {
		err = fmt.Errorf("%s has no branch: only boxes created in a project have one", id)
	}
	return b, err
}

func worktreePath(repo *config.Repo, box string) string {
	return filepath.Join(repo.Root, ".hutch", "worktrees", box)
}

func gitCommands() []*cobra.Command {
	var stat bool
	diff := &cobra.Command{
		Use:     "diff <box>",
		Short:   "Show a box's changes against its base, committed or not",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := boxBranch(ctx, sess, args[0])
			if err != nil {
				return err
			}
			flags := ""
			if stat {
				flags += " --stat"
			}
			if term.IsTerminal(int(os.Stdout.Fd())) {
				flags += " --color=always"
			}
			script := `base=$(git merge-base HEAD "origin/$1") && git diff` + flags + ` "$base" && git ls-files --others --exclude-standard | sed 's/^/untracked: /'`
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: b.ID, Dir: "/workspace", Argv: []string{"sh", "-c", script, "sh", b.Base}}, os.Stdout, os.Stderr)
			if err == nil && code != 0 {
				err = exitError(code)
			}
			return err
		}),
	}
	diff.Flags().BoolVar(&stat, "stat", false, "only a summary per file")

	checkout := &cobra.Command{
		Use:     "checkout <box>",
		Short:   "Check out a box's pushed branch in a local worktree, without waking it",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := boxBranch(ctx, sess, args[0])
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
			if _, err := git(repo.Root, "fetch", "-q", "origin", b.Branch); err != nil {
				return fmt.Errorf("%w (has the box committed anything yet?)", err)
			}
			wt := worktreePath(repo, b.ID)
			if _, err := os.Stat(wt); err == nil {
				if dirty, err := git(wt, "status", "--porcelain"); err != nil {
					return err
				} else if dirty != "" {
					return fmt.Errorf("%s has local changes; commit or discard them first", wt)
				}
				if _, err := git(wt, "merge", "-q", "--ff-only", "origin/"+b.Branch); err != nil {
					return err
				}
			} else if _, err := git(repo.Root, "worktree", "add", "-q", "-B", b.Branch, wt, "origin/"+b.Branch); err != nil {
				return err
			}
			head, _ := git(wt, "log", "-1", "--format=%h %s")
			fmt.Printf("%s\n  at %s\n", wt, head)
			return nil
		}),
	}

	var onto string
	rebase := &cobra.Command{
		Use:     "rebase <box>",
		Short:   "Ask the box's latest thread to rebase its branch on the moved base",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := boxBranch(ctx, sess, args[0])
			if err != nil {
				return err
			}
			if len(b.Threads) == 0 {
				return fmt.Errorf("%s has no threads to ask", b.ID)
			}
			n := b.Threads[len(b.Threads)-1].N
			target := onto
			if target == "" {
				target = "origin/" + b.Base
			}
			if _, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: b.ID, Dir: "/workspace", Argv: []string{"git", "fetch", "-q", "origin"}}, os.Stdout, os.Stderr); err != nil {
				return err
			}
			msg := fmt.Sprintf("The base moved. Rebase this branch (%s) onto %s, which I just fetched; resolve any conflicts, then rerun the tests.", b.Branch, target)
			if err := rpc.Call(ctx, sess, api.ThreadSend, api.SendParams{Box: b.ID, N: n, Message: msg}, nil); err != nil {
				return err
			}
			fmt.Printf("Asked %s/%d to rebase onto %s.\n", b.ID, n, target)
			return nil
		}),
	}
	rebase.Flags().StringVar(&onto, "onto", "", "ref to rebase onto (default: origin/<base>)")

	pr := &cobra.Command{
		Use:     "pr <box> [-- <gh pr create flags>]",
		Short:   "Open a pull request from a box's branch",
		GroupID: "boxes",
		Args:    cobra.MinimumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := boxBranch(ctx, sess, args[0])
			if err != nil {
				return err
			}
			script := `git push -q origin "refs/heads/$1:refs/heads/$1" && gh pr create --head "$1" --base "$2" --fill "${@:3}"`
			argv := append([]string{"bash", "-c", script, "bash", b.Branch, b.Base}, args[1:]...)
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: b.ID, Dir: "/workspace", Argv: argv}, os.Stdout, os.Stderr)
			if err == nil && code != 0 {
				err = exitError(code)
			}
			return err
		}),
	}
	return []*cobra.Command{diff, checkout, rebase, pr}
}

// cleanUpGit removes a removed box's local worktree and, if the user agrees, the
// hutch/<box> branch on origin. Branches Hutch didn't create are never deleted.
func cleanUpGit(b api.Box) error {
	repo, err := config.FindRepo(".")
	if err != nil || repo == nil || repo.ID != b.Project || b.Branch == "" {
		return err
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	wt := worktreePath(repo, b.ID)
	if _, err := os.Stat(wt); err == nil {
		dirty, _ := git(wt, "status", "--porcelain")
		if dirty != "" && !(interactive && confirm(wt+" has local changes. Remove it anyway?")) {
			fmt.Printf("Kept %s.\n", wt)
		} else if _, err := git(repo.Root, "worktree", "remove", "--force", wt); err != nil {
			return err
		} else {
			git(repo.Root, "branch", "-q", "-D", b.Branch)
			fmt.Printf("Removed %s.\n", wt)
		}
	}
	if !strings.HasPrefix(b.Branch, "hutch/") {
		return nil
	}
	if remote, _ := git(repo.Root, "ls-remote", "--heads", "origin", b.Branch); remote == "" {
		return nil
	}
	if !interactive {
		fmt.Printf("Kept %s on origin.\n", b.Branch)
		return nil
	}
	if !confirm(fmt.Sprintf("Delete %s on origin?", b.Branch)) {
		return nil
	}
	if _, err := git(repo.Root, "push", "-q", "origin", "--delete", b.Branch); err != nil {
		return errors.New("delete remote branch: " + err.Error())
	}
	fmt.Printf("Deleted %s on origin.\n", b.Branch)
	return nil
}
