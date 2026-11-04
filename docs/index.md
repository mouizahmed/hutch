# Hutch

Hutch gives every coding agent its own computer, on a Linux machine you rent or own. Each agent works in a box: an isolated environment cloned from a snapshot of your project's setup, with its own files, processes, and services. Terminals keep running when you disconnect, and idle boxes sleep until you come back.

These guides describe the upcoming Hutch v0.2.5; the latest published release is v0.2.4. Hutch is early software and has not had an independent security review; read the [security model](security.md) before giving boxes access to anything you care about.

## What you need

- **A computer** running Linux, macOS, or Windows, for the `hutch` command and its terminal UI.
- **A Linux machine** you reach over SSH, where the boxes run: x86_64, systemd, kernel 6.1 or newer, and passwordless sudo. Machines with KVM run boxes as Firecracker microVMs; other machines use gVisor sandboxes.
- **An agent CLI** with its own login, such as Claude Code or Codex. Other agents work through a TOML adapter.
- Optionally, **an S3-compatible bucket** for backups. Without one, losing the machine loses its boxes.

See [install and requirements](install.md) for the details.

## Where to start

- **New to Hutch:** [install the CLI](install.md), then follow [first box](quickstart.md) from an empty machine to an agent working in a box.
- **Working day to day:** [the TUI](tui.md) shows every project, box, and terminal, and reviews an agent's changes.
- **More than one machine:** [several machines](machines.md) covers adding workers, placing boxes, and what happens when the lead is unreachable.
- **Settings:** [configuration](configuration.md) covers config files, agent logins, MCP servers, and what projects may ask for.

## How it fits together

You run `hutch` on your computer. It talks over SSH to `hutchd`, the daemon on your machine, which keeps your projects and boxes. A project is a Git repository with a `.hutch/` recipe; building that recipe and taking a snapshot makes a template version, and every new box starts as a copy of one. An agent then works in the box on its own branch, and you review and push its work, or bring it back to your computer.

## Releases

The [release notes](changelog.md) list what changed and the current limitations of each version. Hutch is [open source](https://github.com/mouizahmed/hutch) under the Apache-2.0 license.
