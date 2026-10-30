# One machine

Hutch runs one daemon, `hutchd`, on your Linux machine. You reach it through SSH; no public box ports or web login are required.

## Prepare the machine

Use an x86_64 Linux machine with systemd and kernel 6.1 or newer. The CLI also runs on arm64 computers, but arm64 machines are not supported yet. An Ubuntu machine with `apt` is the simplest path. Eight GB of RAM is a practical starting point; leave space for your projects and their snapshots. `/dev/kvm` selects Firecracker; without it Hutch uses gVisor. Their isolation and supported workloads differ; see [security](security.md).

Make `ssh <ssh-host>` work with a key, and allow that SSH user passwordless sudo. Run on your own computer:

```sh
hutch setup <ssh-host>
```

A released CLI downloads checksum-verified machine files for its exact version and the machine's architecture. A development CLI uses `make linux images` output; `--hutchd` and `--images` select local files explicitly.

Setup checks the machine, proposes storage, and installs the runtimes and daemon. `/var/lib/hutch` must use XFS with reflink. Review any disk-format prompt carefully. It also offers an S3-compatible backup bucket. Without one, nothing survives losing the machine. A bucket's recovery passphrase must be saved somewhere you can recover independently of that machine.

```sh
hutch doctor
hutch machine
```

## Add your tools and project

Log in with each tool's own flow, then review what Hutch will copy from your computer:

```sh
hutch login claude gh                  # choose claude or codex
hutch user sync
hutch user status
```

Config sync affects newly created boxes. A copied MCP command still needs its executable, packages, files, and network access inside the box. OAuth for an MCP server is separate from the agent's login. See [configuration](configuration.md).

In a Git checkout with an origin remote:

```sh
hutch init
```

Use `SETUP_PROMPT.md` to prepare `.hutch/` with your agent. Review its setup and verification scripts, image, resource sizes, and requested access. Commit and push the recipe, then:

```sh
hutch template build
hutch template snapshot
hutch new "Run the tests"
```

A build prepares the environment; snapshot publishes the version new boxes use. Existing boxes keep their own environment. The template agent's work is imported for review during a build; resolve conflicts before replacing its box.

## Work and reconnect

```sh
hutch                                # open the TUI
hutch ls
hutch attach <box>
hutch shell <box>
hutch exec <box> -- git status
hutch diff <box>
```

Each box is an independent working environment. Terminals in one box share files, dependencies, and services. A terminal is a process session; the agent's conversation is managed by the agent itself. Cold boots leave saved agent terminals ready to resume through the agent's picker.

For a service listening on port 3000 in a box:

```sh
hutch forward <box> 3000
```

Use the local address printed by that command. The listener remains while the box sleeps; a connection wakes it and waits for a bounded interval. Open forwarded connections keep the box awake. The service may need its own startup or readiness handling after a cold boot.

## Update and recover

Run the installer again to select the newest published CLI, or pin `HUTCH_VERSION` to an existing release. Then:

```sh
hutch machine upgrade
hutch doctor
```

For a lost machine backed by a bucket, use `hutch machine restore <new-ssh-host>` with its recovery passphrase. Avoid deleting a box until its work is somewhere you intend to keep it. [Release validation](releases.md) tracks checks still needed before public release, including live upgrade continuity and both backends.
