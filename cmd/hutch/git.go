package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
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

// boxBranch returns a box with a git repository: one created in a project.
func boxBranch(ctx context.Context, sess *yamux.Session, id string) (api.Box, error) {
	b, err := findBox(ctx, sess, id)
	if err == nil && b.Base == "" {
		err = fmt.Errorf("%s has no repository: only boxes created in a project have one", id)
	}
	return b, err
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

	pr := &cobra.Command{
		Use:     "pr <box> [-- <gh pr create flags>]",
		Short:   "Push a box's branch and open a pull request from it",
		GroupID: "boxes",
		Args:    cobra.MinimumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := boxBranch(ctx, sess, args[0])
			if err != nil {
				return err
			}
			// Uncommitted work would be left out of the PR without a word, so it's
			// the agent's or your call what to commit.
			script := `branch=$(git symbolic-ref --short -q HEAD) || { echo "HEAD is detached in the box; check out a branch first" >&2; exit 3; }
if [ "$branch" = "$1" ]; then echo "the box is on $1, the default branch; a pull request needs a branch of its own (git switch -c <name> in the box)" >&2; exit 3; fi
if [ -n "$(git status --porcelain)" ]; then
  echo "the box has uncommitted changes; commit them first (or ask the agent to):" >&2
  git status --short >&2
  exit 3
fi
git push -q -u origin "refs/heads/$branch:refs/heads/$branch" && gh pr create --head "$branch" --base "$1" --fill "${@:2}"`
			argv := append([]string{"bash", "-c", script, "bash", b.Base}, args[1:]...)
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: b.ID, Dir: "/workspace", Argv: argv}, os.Stdout, os.Stderr)
			if err == nil && code != 0 {
				err = exitError(code)
			}
			return err
		}),
	}
	return []*cobra.Command{diff, pr}
}
