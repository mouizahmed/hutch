# How Hutch works

This page explains the pieces Hutch is made of, what runs where, and how a box gets from your repository to an agent at work. The guides use these words the same way throughout. For the code behind it, see [architecture](architecture.md).

## Computer and machine

Your **computer** is where you type: it runs the `hutch` command and its terminal UI, and holds your own checkouts. A **machine** is the Linux server that runs boxes, with the `hutchd` daemon on it. Your computer reaches the machine over SSH; nothing on the machine is published to the internet for you.

With more than one machine, one is the **lead**: it keeps the state, places boxes, and answers the CLI. The others are **workers** that run boxes for it, and dial out to the lead, so they need no open port. See [several machines](machines.md).

```text
 your computer          the lead machine                               a worker machine
┌─────────────┐  SSH  ┌──────────────────────────────────────┐  link  ┌──────────────────┐
│ hutch       │ ────▶ │ hutchd: state, placement, backups    │ ◀───── │ hutchd           │
│ CLI and TUI │       │                                      │  7443  │                  │
└─────────────┘       │  ┌─────────────────┐  ┌───────────┐  │        │  ┌────────────┐  │
                      │  │ box             │  │ box       │  │        │  │ box        │  │
                      │  │  init, services │  │  ...      │  │        │  │  ...       │  │
                      │  │  tmux terminals │  │           │  │        │  └─────┬──────┘  │
                      │  └────────┬────────┘  └───────────┘  │        │        │         │
                      │           ▼ HTTPS                    │        │        ▼         │
                      │  keys proxy: placeholders → logins   │        │   keys proxy     │
                      └───────────┬──────────────────────────┘        └────────┬─────────┘
                                  └───────────── bucket (backups) ─────────────┘
```

## Projects, templates, and boxes

A **project** is a Git repository with an `origin` the machine can clone. Hutch knows it by its remote, so every checkout of the same repository is the same project.

A project's **recipe** is its `.hutch/` directory: a Dockerfile for the environment, optional services, and scripts that install dependencies and check that the project works. Building the recipe runs it in a fresh box, the **template box**; taking a **snapshot** of that box makes a numbered **template version**. See [projects and templates](templates.md).

A **box** is one agent's computer. New boxes start as copies of the newest template version, so they come up with dependencies installed and services running, in seconds. The copy is cheap: its disk is a copy-on-write clone of the template's, and so is its memory, so a new box only uses space for what it changes. Each box has its own files, processes, and services, and its own branch, `hutch/<box>`, checked out at `/workspace`. Changing one box never changes another, or the template. See [boxes and terminals](boxes.md).

## Inside a box

A box is a whole Linux system, whatever its image. Hutch's own init, `hutch-guest`, runs it in place of systemd: it starts the commands in the project's `.hutch/services` and restarts them if they exit, runs each terminal in tmux, and carries out the commands Hutch sends it, like `hutch exec`.

You and the agents work as `dev`, with passwordless sudo. Root inside a box controls that box, and only that box.

## Firecracker and gVisor

Hutch runs boxes with one of two runtimes, chosen per machine:

| | Firecracker | gVisor |
| --- | --- | --- |
| What a box is | A microVM: a real virtual machine with its own Linux kernel | A sandbox: your programs run against a Linux kernel implemented in user space, with no VM |
| Needs | `/dev/kvm` on the machine | Nothing special |
| Isolation | The VM boundary, by KVM, with Firecracker run in a jail | gVisor's boundary, which intercepts every system call |
| The box's kernel | Hutch's guest kernel (`uname -r`: `6.1.…-hutch`) | gVisor's (`uname -r`: `4.19.0-gvisor`) |
| Speed | Close to the machine's | Builds and system-call-heavy work run slower: setup estimates about 1.5× for builds |

Setup picks Firecracker when the machine has `/dev/kvm`, and gVisor when it doesn't. Bare-metal servers have KVM; most cloud instances don't, unless their type supports nested virtualization and it's turned on.

gVisor implements most of Linux, but not all of it, so a program that depends on unusual kernel features, or on kernel modules, may behave differently or fail. Run your project's `verify.sh` on the machine you'll use: a template build that passes there is the test that matters. See the [security model](security.md) for how the two isolate boxes.

Machines of different runtimes can work together: boxes are placed on any machine that fits. What can't cross is a box's memory, as the next section explains.

## How logins reach a box

Boxes never hold your real logins. When you sign in with `hutch login`, the tokens stay on the machine, encrypted, and each box gets placeholders in their place.

A box's HTTPS traffic goes through the **keys proxy** on its machine. When a request goes to one of an agent's own hosts, like `anthropic.com` for Claude, the proxy swaps the placeholder for the real token. Toward any other host, the placeholder is passed on unchanged and authenticates nothing. Token refreshes happen on the lead, one at a time per login, so one box refreshing a login doesn't sign the others out.

This keeps a login from being copied out of a box, but a box can still use it while it runs, for whatever the account allows. See [agents, logins, and MCPs](agents.md) and the [security model](security.md#credentials).

## Terminals and agents

A box runs any number of **terminals**. Each is a program in its own terminal session: an agent like Claude Code or Codex, a shell, or any command. Terminals in one box share its files and services, so one can run the tests while another writes code.

A terminal keeps running when you close the TUI or your computer sleeps, because it runs in tmux inside the box, not on your computer; attaching again shows it as it is now. An agent's **conversation** is the agent's own: it lives in the agent's session files, not in the terminal. If a terminal's process ends, the conversation can be resumed in a new one with the agent's own picker.

## Sleep and wake

A box with nothing happening for `idle_timeout` (10 minutes by default) **sleeps**: Hutch saves a snapshot of it, memory included, and stops it, so it stops using memory and CPU. A box counts as active while its CPU use is above `idle.cpu_threshold` (5% by default), as an agent's is while it works, while you're attached to one of its terminals, and while a forwarded connection to it is open. An agent waiting for you at its prompt doesn't keep a box awake.

Waking a box is usually **warm**: its memory comes back, and every program carries on where it was, mid-command. Sometimes a wake is **cold**: the box boots from its disk, so files are kept but processes start again, and agent terminals wait at the agent's session picker. A wake is cold:

- after a box has slept for `archive_after` (30 days), when its memory is dropped to save space;
- after a resize, since its memory no longer fits;
- after a move to a machine of a different **snapshot class**.

A snapshot class is a runtime and its version, and the CPU vendor a memory snapshot depends on; gVisor snapshots also check the CPU's features. A box's memory restores only on a machine of the same class: a Firecracker box can't wake warm under gVisor, nor on a CPU from the other vendor. `hutch machine` shows each machine's class.

## Where your data lives

| Where | What | Without it |
| --- | --- | --- |
| Your computer | Your config, Hutch's SSH key, the recovery file, and your own checkouts | Install Hutch again and run `hutch machine join`; nothing on the machine is lost |
| Each machine's disk | Its boxes: their disks, memory snapshots, and template versions | Without a bucket, its boxes are gone |
| The lead | Hutch's state: projects, boxes, logins, uploaded files, and settings, encrypted | Restore it from the bucket |
| The bucket | Encrypted copies of all of the above but your computer's | Losing a machine loses its boxes |

A **snapshot** is a box saved on its machine, for sleeping and waking, and for template versions. A **backup** is a copy in the bucket, for when a machine is lost: awake boxes are checkpointed to it every `checkpoint.interval` (15 minutes), and the lead's state every `backup.interval` (1 minute) when it changes. See [backups and recovery](backups.md).

## Your work and Git

An agent works on its box's branch, committing as it goes. Nothing leaves the box until you push it: review with `hutch diff` or the TUI's Git view, then push the branch or open a pull request, or bring it back to your own checkout with `hutch local`. See [review and deliver work](git.md) and [move between local and cloud](handoff.md).
