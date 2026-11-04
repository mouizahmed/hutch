# Review and deliver work

An agent's work stays in its box until you decide where it goes: a pull request, a push, or your own checkout. This page covers the box's branch, reviewing changes, and getting them out.

## The box's branch

Every box in a project works on its own branch, `hutch/<box>`, checked out at `/workspace`. It starts from the project's **base**: the repository's default branch, or `base` in `hutch.toml`. Agents commit as they go, with your Git name and email from `hutch user sync`.

Nothing leaves the box on its own. Pushing, fetching, and opening pull requests are each something you do; looking at the work never contacts a remote.

## Review

```sh
hutch diff <box>          # everything since the base, committed or not, and untracked files
hutch diff <box> --stat   # a summary per file
```

The TUI's Git view (`g` on a box) goes further: each file's lines, the commits not yet pushed, every file's diff, and the task diff against the base (Tab). From there you can stage whole files or single hunks, and commit what's staged. Staging a hunk is refused if its file changed since you looked, and a commit is refused if what's staged changed, so you never commit something you haven't seen. See [the TUI](tui.md#git-g).

To look around the box yourself:

```sh
hutch exec <box> -- git log --oneline origin/main..HEAD
hutch shell <box>
```

## Open a pull request

```sh
hutch pr <box>
hutch pr <box> -- --draft --title "Fix the flaky login test"
```

This pushes the box's branch to `origin` and runs `gh pr create` inside the box, against the base, with the commits' messages as the description. Arguments after `--` go to `gh pr create`. It needs the GitHub login (`hutch login gh`).

It's refused, with the reason, if the box has uncommitted changes (commit them, or ask the agent to), if its `HEAD` is detached, or if it's on the base branch itself. Uncommitted work would otherwise be left out without a word.

## Push without a pull request

In the TUI's Git view, `P` pushes the box's branch, and sets its upstream the first time; `f` fetches and `p` pulls, fast-forward only. Each asks first. From the command line:

```sh
hutch exec <box> -- git push -u origin HEAD
```

## Carry on locally

To bring the branch, and the agents' conversations, into your own checkout:

```sh
hutch local <box>
```

See [move between local and cloud](handoff.md).

## Before removing a box

`hutch rm` deletes the box and everything only in it: unpushed commits and uncommitted changes. The sidebar shows a box's unpushed lines, like `+17 −1`, and the Git view lists its unpushed commits; push them, or bring them home with `hutch local`, first.
