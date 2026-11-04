# Boxes and terminals

A box is one agent's computer: a copy of the project's template with its own files, processes, services, and branch. This page covers making boxes, working in their terminals, and sleeping, moving, resizing, and removing them. Everything here is also in [the TUI](tui.md).

## Make a box

In a checkout of a project with a template:

```sh
hutch new "Fix the flaky login test"
```

The box is a copy of the newest template version, on a new branch `hutch/<box>` from the project's default branch (or `base` in `hutch.toml`). The agent starts in `/workspace` with your prompt. Boxes get a generated name like `quiet-fox` unless you give one with `--name`.

| Flag | |
| --- | --- |
| `--agent codex` | Run this agent instead of `default_agent`. |
| `--model opus` | The agent's model, in the agent's own names. |
| `--cmd bash` | Run any command instead of an agent. |
| `--on <machine>` | Start it on this machine; it waits there while that machine is full. |
| `--from <box>` | Copy a sleeping box instead of the template: its files and checkout as they are, on its machine. |
| `--dry-run` | Say which template, logins, and network access the box would get, and start nothing. |

Outside a project, or with `--image base`, a box boots from an image instead of a template: no checkout, nothing installed beyond the image. Such a box has no agent CLIs either, so start it with `--cmd bash`:

```sh
hutch new --image base --cpus 1 --mem 1024 --cmd bash --name scratch
```

If the machines are full, or `limits.awake_boxes` (8) boxes are already awake, a new box queues and starts when there's room. `hutch ls` shows why it's waiting.

## Terminals

A box has one terminal to start with, and any number more:

```sh
hutch terminal <box> "Write tests for the parser"     # another agent
hutch terminal <box> --agent codex "Review the diff"  # a different one
hutch terminal <box> --cmd "npm run dev"              # any command
hutch shell <box>                                     # bash, and attach to it
```

Terminals in one box share its files and services. Each keeps running when nothing is attached to it.

```sh
hutch attach <box>        # its terminals; wakes it if it's asleep
hutch attach <box>/2      # one terminal
```

Attaching opens the box's terminals in tmux: switch between them with `Ctrl-b n` and `Ctrl-b p`, and detach with `Ctrl-b d`. Everything keeps running.

To work with terminals without attaching:

```sh
hutch send <box>/1 "Also update the changelog"   # type a message into an agent
hutch terminal rename <box>/2 "dev server"       # your name, over the title its program sets
hutch terminal rm <box>/2                        # close it, and what runs in it
```

## Look inside

```sh
hutch ls                          # boxes, their terminals, and what each is doing
hutch status <box>                # one box: branch, machine, terminals
hutch exec <box> -- git status    # one command, as dev; its output and exit code
hutch exec <box> --user root -- apt-get install -y jq
hutch diff <box>                  # its changes against its base, committed or not
hutch console <box>               # its serial console, even when the box is broken
```

`hutch ls` shows each terminal's state: working, waiting for you, or exited. `hutch events` prints the same as JSON lines, as things change, for scripts.

## Sleep and wake

Boxes sleep on their own after `idle_timeout` without activity, and wake when you attach, exec, or connect to a forwarded port. To do it yourself:

```sh
hutch sleep <box>
hutch wake <box>
```

A warm wake brings back memory too, so programs carry on mid-command; a cold one boots from the disk. See [how Hutch works](concepts.md#sleep-and-wake) for when each happens. A box awake for `limits.box_max_awake` (8 hours) without a break is put to sleep even mid-task.

## Move and resize

```sh
hutch move <box> --to <machine>
hutch resize <box> --cpu 8 --memory 16G --disk 100G
```

A move is warm between machines of the same snapshot class, and cold otherwise. A resize is always a cold boot: running processes restart, and each agent's terminal waits at its session picker. CPU and memory can go up or down; the disk only grows. A box that no longer fits its machine moves to one that does.

## Remove a box

```sh
hutch rm <box>
```

This deletes the box and all its snapshots, at once and without asking. Work that's only in the box goes with it: commits that weren't pushed, and changes that weren't committed. Push the branch first, or bring it to your checkout with `hutch local`; see [review and deliver work](git.md).

## Name, branch, and base

| | |
| --- | --- |
| Branch | `hutch/<box>`, at `/workspace` |
| Base | The project's default branch, or `base` in `hutch.toml`; `hutch diff` and the task diff compare against it. |
| User | `dev`, with passwordless sudo |
| Network | Outbound internet; inbound only through Hutch, with [forwarded ports and SSH](networking.md) |
