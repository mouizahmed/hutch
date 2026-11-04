# Move between local and cloud

You can start work on your computer and move it into a box, or bring a box's work back to your checkout, with the agent's conversation coming along both ways. This page covers `hutch cloud` and `hutch local`.

## Send your work to a box

In a checkout of a project with a template, with an agent conversation you'd like to carry on in a box:

```sh
hutch cloud
```

A new box starts from the project's template, and gets your work exactly as it is here:

- your branch at `HEAD`, including commits not yet pushed;
- staged and unstaged changes, and untracked files that Git doesn't ignore.

Then it finds the newest agent conversation started in this directory, Claude Code's or Codex's, copies it in, and resumes it in the box. It prints which session it took and how long ago it was written. If the agent may still be running here, because its session was written in the last 30 seconds, it says so: quit it, since anything it does from now on stays on your computer.

Last, it stashes your local changes, as `hutch cloud <box>`, since the box has them now. Drop the stash once you're done with the box's version. You can close your laptop.

| Flag | |
| --- | --- |
| `--agent claude` | Only take this agent's conversation. |
| `--no-session` | Move only the code, and start a new agent. |
| `--name <box>` | Name the box. |
| `<dir>` | Move from this directory instead of the current one; agents record the directory they ran in. |

Ignored files, like `node_modules` or `.env`, don't move: the box has the template's. List files a box needs but Git mustn't have in [`uploads.txt`](templates.md#files-that-arent-in-git). Changes and conversations are limited to 32 MB; commit and push large files first.

`hutch cloud` requires a project with a built template. Set one up first with `hutch init` in the Git repository, then `hutch template build`, as in [projects and templates](templates.md). Outside a project or before its first template build, it stops with those steps to follow.

## Bring a box's work back

In a checkout of the box's project:

```sh
hutch local <box>
```

This:

1. commits the box's uncommitted work, as one commit named `WIP (hutch)`;
2. fetches the box's branch into your repository over Hutch's connection, without a remote;
3. checks out that branch here, as `hutch/<box>`;
4. copies every conversation of the box's agents to your computer, with their paths rewritten to your checkout;
5. puts the box to sleep.

It ends with the command to carry on each agent's conversation here, which opens the agent's own session picker.

Your checkout must be clean first: commit or stash your changes. If you already have a `hutch/<box>` branch that's moved on, nothing is changed, and it tells you how to merge the box's work yourself.

The box keeps its work: wake it to carry on there, or remove it once your checkout has everything.

## On Windows

On native Windows, `hutch cloud` and `hutch local` move code but not conversations: a box starts a new one, and its conversations stay in the box. In WSL, conversations move as on Linux and macOS.

## Which agents

Conversations move for agents whose session format Hutch reads: Claude Code and Codex. Other agents' code moves the same way, and they start a new conversation.
