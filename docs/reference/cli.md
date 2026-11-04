# CLI reference

Every `hutch` command and flag, generated from the commands themselves. `hutch <command> --help` shows the same for the version you have installed. With no command, `hutch` opens [the TUI](../tui.md).

## Boxes

### hutch attach

Attach to a box's terminals (wakes it if asleep).

```text
hutch attach <box>[/<terminal>]
```

### hutch cloud

Move this work and its agent session into a new box.

```text
hutch cloud [<dir>] [flags]
```

| Flag | |
| --- | --- |
| `--agent <string>` | only hand over this agent's session |
| `--name <string>` | box name (default: generated) |
| `--no-session` | move only the code; start a new agent |

### hutch console

Show a box's serial console (works when the guest is broken).

```text
hutch console <box> [flags]
```

| Flag | |
| --- | --- |
| `-f`, `--follow` | keep printing new output |
| `-i`, `--interactive` | type into the console of a running box (root shell) |

### hutch diff

Show a box's changes against its base, committed or not.

```text
hutch diff <box> [flags]
```

| Flag | |
| --- | --- |
| `--stat` | only a summary per file |

### hutch events

Print box, terminal, and worker changes as JSON lines: first everything as it is now, then a {"type":"synced"} line, then each change as it happens. Types: box, thread, worker (the whole object as it is now), box_removed, thread_removed, worker_removed (with id, and n for a thread), and alert (a terminal whose agent rang the bell or exited while nobody was looking).

```text
hutch events
```

### hutch exec

Run one command in a box; output and exit code.

```text
hutch exec <box> -- <command> [args...] [flags]
```

| Flag | |
| --- | --- |
| `--user <string>` | user to run as (default `dev`) |

### hutch forward

Reach a box's ports on localhost; with no ports, each port it listens on.

```text
hutch forward <box> [<port>[:<local port>]...]
```

### hutch local

Bring a box's branch and agent sessions into this checkout; the box sleeps.

```text
hutch local <box>
```

### hutch ls

List boxes.

```text
hutch ls
```

### hutch move

Move a box to another machine (warm when it can, else cold).

```text
hutch move <box> [flags]
```

| Flag | |
| --- | --- |
| `--to <string>` | machine to move to (default: the scheduler's choice) |

### hutch new

Create a box and start an agent in it.

```text
hutch new ["<prompt>"] [flags]
```

| Flag | |
| --- | --- |
| `--agent <string>` | agent to run: claude, codex (default: default_agent) |
| `--cmd <string>` | run any terminal command instead of an agent |
| `--cpus <int>` | vCPUs (with `--image`; default: size.cpu) |
| `--dry-run` | print what would happen (template, logins, network) and start nothing |
| `--from <string>` | sleeping box to fork from (default: this project's template; outside a project, the base image) |
| `--image <string>` | cold boot from this image on the machine instead of forking |
| `--mem <int>` | memory in MiB (with `--image`; default: size.memory) |
| `--model <string>` | the agent's model, like opus or gpt-6.1-sol (default: the agent's own) |
| `--name <string>` | box name (default: generated, like quiet-fox) |
| `--on <string>` | the machine to start it on (hutch machine lists them); it queues there while that machine is full |

### hutch pr

Push a box's branch and open a pull request from it.

```text
hutch pr <box> [-- <gh pr create flags>]
```

### hutch resize

Change one box's size. It applies with a cold boot: running processes restart, and each agent's thread waits at its exit, a key from its conversation picker. CPU and memory can go up or down; the disk only grows. If the box no longer fits its worker, it moves to one that fits.

```text
hutch resize <box> [flags]
```

| Flag | |
| --- | --- |
| `--cpu <int>` | vCPUs |
| `--disk <string>` | disk, like 100G (only grows) |
| `--memory <string>` | memory, like 16G |

### hutch rm

Delete a box and its snapshots.

```text
hutch rm <box>
```

### hutch send

Type a message into an agent's terminal.

```text
hutch send <box>/<terminal> "<message>"
```

### hutch shell

Open a terminal running bash in a box, and attach to it.

```text
hutch shell <box>
```

### hutch sleep

Snapshot a box and stop its VM.

```text
hutch sleep <box>
```

### hutch status

Show a box, its branch, and its terminals.

```text
hutch status <box>
```

### hutch terminal

Start another terminal in a box: an agent, a shell (`--cmd` bash), or a command.

```text
hutch terminal <box> ["<prompt>"] [flags]
```

Also: `thread`.

| Flag | |
| --- | --- |
| `--agent <string>` | agent to run: claude, codex (default: default_agent) |
| `--cmd <string>` | run any terminal command instead of an agent |
| `--model <string>` | the agent's model, like opus or gpt-6.1-sol (default: the agent's own) |

### hutch terminal rename

Give a terminal your own name, over the title its program sets.

```text
hutch terminal rename <box>/<terminal> "<name>"
```

### hutch terminal rm

Close a terminal, and what runs in it.

```text
hutch terminal rm <box>/<terminal>
```

### hutch wake

Restore a sleeping box.

```text
hutch wake <box>
```

## Projects and templates

### hutch init

Set up this repository: a template box and a prompt for your agent.

```text
hutch init
```

### hutch project

Manage projects on the machine.

### hutch project rm

Remove a project's templates and secrets from the machine (default: this repository).

```text
hutch project rm [<project>] [flags]
```

| Flag | |
| --- | --- |
| `--yes` | don't ask |

### hutch sizes

Box size options and how many fit on your machine.

```text
hutch sizes [flags]
```

| Flag | |
| --- | --- |
| `--json` | machine-readable output |

### hutch template

Build and manage this project's template.

### hutch template build

Run the committed recipe in a fresh VM and verify it.

```text
hutch template build [flags]
```

| Flag | |
| --- | --- |
| `--commit` | commit .hutch/ changes, merge the template box's work, and push without asking |
| `--cpu <int>` | vCPUs of every box (skips the project form) |
| `--disk <string>` | disk of every box, like 50G (skips the project form) |
| `--idle <string>` | idle timeout, like 10m (skips the project form) |
| `--memory <string>` | memory of every box, like 8G (skips the project form) |
| `--yes` | accept the project settings as they are |

### hutch template shell

Open a shell on the template box.

```text
hutch template shell
```

### hutch template snapshot

Freeze the recipe at HEAD as a new template version (builds it if needed).

```text
hutch template snapshot [flags]
```

| Flag | |
| --- | --- |
| `--commit` | commit .hutch/ changes, merge the template box's work, and push without asking |
| `--cpu <int>` | vCPUs of every box (skips the project form) |
| `--disk <string>` | disk of every box, like 50G (skips the project form) |
| `--idle <string>` | idle timeout, like 10m (skips the project form) |
| `--memory <string>` | memory of every box, like 8G (skips the project form) |
| `--yes` | accept the project settings as they are |

### hutch template versions

List template versions.

```text
hutch template versions
```

## Setup

### hutch config

Show and change settings.

### hutch config edit

Edit one layer in your editor (`--project` opens the project form).

```text
hutch config edit [flags]
```

| Flag | |
| --- | --- |
| `--cpu <int>` | vCPUs of every box (skips the project form) |
| `--disk <string>` | disk of every box, like 50G (skips the project form) |
| `--idle <string>` | idle timeout, like 10m (skips the project form) |
| `--machine` | the machine config, for every machine |
| `--memory <string>` | memory of every box, like 8G (skips the project form) |
| `--on <string>` | one machine's own machine config, over every machine's |
| `--project` | the project's .hutch/hutch.toml |
| `--user` | your config (the default) |
| `--yes` | accept the project settings as they are |

### hutch config explain

Print the effective config for this project, with sources.

```text
hutch config explain
```

### hutch config get

Print a setting's effective value and where it comes from.

```text
hutch config get <key> [flags]
```

| Flag | |
| --- | --- |
| `--on <string>` | as one machine has it |

### hutch config grants

Show what you've granted projects, or revoke it.

```text
hutch config grants [<project>] [flags]
```

| Flag | |
| --- | --- |
| `--revoke` | remove everything granted to the project |

### hutch config set

Change a setting in one layer (lists are comma-separated).

```text
hutch config set <key> <value> [flags]
```

| Flag | |
| --- | --- |
| `--machine` | the machine config, for every machine |
| `--on <string>` | one machine's own machine config, over every machine's |
| `--project` | the project's .hutch/hutch.toml |
| `--user` | your config (the default) |

### hutch config sync

Sync your config with the machine's copy (runs before every command).

```text
hutch config sync [flags]
```

| Flag | |
| --- | --- |
| `--mine` | replace the machine's copy with this computer's |
| `--theirs` | replace this computer's copy with the machine's |

### hutch config unset

Remove a setting from one layer, so the layers below it apply.

```text
hutch config unset <key> [flags]
```

| Flag | |
| --- | --- |
| `--machine` | the machine config, for every machine |
| `--on <string>` | one machine's own machine config, over every machine's |
| `--project` | the project's .hutch/hutch.toml |
| `--user` | your config (the default) |

### hutch config validate

Check every layer against the schema.

```text
hutch config validate
```

### hutch doctor

Check this computer, the machine, its bucket, boxes, and config, with a fix for each problem.

```text
hutch doctor [flags]
```

| Flag | |
| --- | --- |
| `--template` | build the committed project twice in temporary boxes, compare fingerprints, and check declared MCP servers |

### hutch login

Log in to agents and GitHub inside a box; every box gets the logins.

```text
hutch login [<agent>...] [flags]
```

| Flag | |
| --- | --- |
| `--from <string>` | save logins already made in this box instead of logging in again |

### hutch machine

Your machines: what they run and how full they are.

```text
hutch machine
```

### hutch machine add

Add another machine you reach over SSH. It gets hutchd and the machine checks, then dials this machine (the lead) on its link port, so it needs no open port of its own. Several machines need a bucket.

```text
hutch machine add <ssh-host> [flags]
```

| Flag | |
| --- | --- |
| `--disk <string>` | blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file) |
| `--force` | install even when the machine runs this or a newer hutchd |
| `--hutchd <string>` | local hutchd binary for the machine (default: download the matching release, or use build output) |
| `--image-size <int64>` | size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space) |
| `--images <string>` | local directory with `vmlinux-<arch>`, `tmux-<arch>` and terminfo.tar |
| `--name <string>` | the machine's name (default: the SSH host, else its hostname) |
| `--yes` | take every recommended answer without asking |

### hutch machine add-computer

Prints the command to run on another computer to use this machine from it. Given that computer's SSH public key (hutch machine join prints it when it can't connect), it first lets the key in on the machine.

```text
hutch machine add-computer [<public key>]
```

### hutch machine bucket

Back the machine up to a bucket, and set the recovery passphrase.

```text
hutch machine bucket
```

### hutch machine forget-stale

Delete the lead state an old lead kept aside, once you've checked the new lead.

```text
hutch machine forget-stale <ssh-host>
```

### hutch machine gc

Delete old box snapshots and bucket chunks nothing uses.

```text
hutch machine gc
```

### hutch machine identity

Print the machine's issuer and public keys for verifying box identities.

```text
hutch machine identity
```

### hutch machine join

Use a machine that another of your computers set up.

```text
hutch machine join <ssh-host>
```

### hutch machine lead

Make another machine the lead.

`<machine>` is a connected worker's name: changes and login refreshes stop for seconds, the lead hands it the database and master key, and the old lead becomes its worker. Boxes keep running; nothing is lost.

With `--from-backup`, for a lead that's dead, `<machine>` is the SSH host of a worker to promote: it restores the lead's newest backup from the bucket, with your recovery passphrase, and leads. Up to backup.interval of changes can be lost. The old lead is fenced over SSH first, so it can't keep leading; if it can't be reached, `--down` confirms it's off or cut off from the bucket and the other machines. Its boxes come back elsewhere when used, and it rejoins as a worker with hutch machine add.

```text
hutch machine lead <machine> [flags]
```

| Flag | |
| --- | --- |
| `--down` | with `--from-backup`: go on when the old lead can't be fenced over SSH, as it's off |
| `--from-backup` | the lead is dead: promote `<machine>` (its SSH host) from the bucket's newest backup |

### hutch machine restore

Set up a new machine and restore your old machine's state onto it from the bucket, with your recovery passphrase. Boxes come back from their newest snapshots the first time you use them. Then the new machine is your machine.

```text
hutch machine restore <ssh-host> [flags]
```

| Flag | |
| --- | --- |
| `--disk <string>` | blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file) |
| `--force` | install even when the machine runs this or a newer hutchd |
| `--hutchd <string>` | local hutchd binary for the machine (default: download the matching release, or use build output) |
| `--image-size <int64>` | size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space) |
| `--images <string>` | local directory with `vmlinux-<arch>`, `tmux-<arch>` and terminfo.tar |
| `--yes` | take every recommended answer without asking |

### hutch machine rm

Move a machine's boxes to the others, then remove it; it's refused from then on.

With `--lost`, for a machine that's gone: its boxes come back elsewhere from their newest snapshots in the bucket (work since then is lost). First the machine is fenced over SSH, so it can't keep running its copies: its hutchd restarts with every box asleep. If it can't be reached, `--down` confirms it's off or cut off from the bucket and the other machines (stopped, unplugged, firewalled); Hutch can't check that for you, and a machine that's still running its boxes would keep two copies of each working.

```text
hutch machine rm <name> [flags]
```

| Flag | |
| --- | --- |
| `--down` | with `--lost`: go on when the machine can't be fenced over SSH, as it's off |
| `--lost` | the machine is gone: bring its boxes back elsewhere from the bucket |

### hutch machine upgrade

Install this version of hutchd on the lead, then on every other machine, and restart them. Running boxes are checkpointed first and keep running; each worker reconnects within seconds. A worker that can't be reached over SSH is skipped and named.

```text
hutch machine upgrade [flags]
```

| Flag | |
| --- | --- |
| `--disk <string>` | blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file) |
| `--force` | install even when the machine runs this or a newer hutchd |
| `--hutchd <string>` | local hutchd binary for the machine (default: download the matching release, or use build output) |
| `--image-size <int64>` | size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space) |
| `--images <string>` | local directory with `vmlinux-<arch>`, `tmux-<arch>` and terminfo.tar |
| `--yes` | take every recommended answer without asking |

### hutch setup

Set up a machine you reach over SSH to run your boxes. It checks the machine, installs what's missing (packages, Firecracker, gVisor, BuildKit, hutchd as a service, the default image), makes it your machine, offers a bucket, and ends with hutch doctor. Run it again to update hutchd; boxes and state are kept.

```text
hutch setup <ssh-host> [flags]
```

| Flag | |
| --- | --- |
| `--disk <string>` | blank disk to format for /var/lib/hutch (default: the only blank disk; none: an image file) |
| `--force` | install even when the machine runs this or a newer hutchd |
| `--hutchd <string>` | local hutchd binary for the machine (default: download the matching release, or use build output) |
| `--image-size <int64>` | size in GB of /var/lib/hutch.img when there's no blank disk (default: 80% of free space) |
| `--images <string>` | local directory with `vmlinux-<arch>`, `tmux-<arch>` and terminfo.tar |
| `--no-boxes` | a machine that only coordinates (machine.run_boxes = false): no KVM, XFS, or box runtimes needed |
| `--no-bucket` | no bucket for now (add one later with hutch machine bucket) |
| `--yes` | take every recommended answer without asking |

### hutch ssh-config

Add `<box>.hutch` hosts to ~/.ssh/config, for ssh, VS Code, and JetBrains.

```text
hutch ssh-config [flags]
```

| Flag | |
| --- | --- |
| `--print` | print the entry instead of writing it |

### hutch user

Your agent config, MCP servers, and logins for every box.

### hutch user status

Show what the user layer holds.

```text
hutch user status
```

### hutch user sync

Copy agent config and MCP servers from this computer into the user layer.

```text
hutch user sync [flags]
```

| Flag | |
| --- | --- |
| `--yes` | apply without asking |
