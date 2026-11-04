# Architecture

This page is for contributors: how Hutch's parts talk to each other, and where each lives in the code. For building and testing, see [contributing](../CONTRIBUTING.md); for the user's view of the same pieces, see [how Hutch works](concepts.md).

## The pieces

```text
 computer                  lead machine                         worker machine
┌───────────┐  SSH  ┌──────────────────────────────┐  link  ┌──────────────────────┐
│ hutch     │ ────▶ │ hutchd                       │ ◀───── │ hutchd               │
│ CLI + TUI │       │  server: state, placement,   │  mTLS  │  agent + worker      │
└───────────┘       │    login signing, backups    │  7443  │  keys proxy          │
                    │  agent + worker: its boxes   │        │                      │
                    │  keys proxy                  │        │  ┌────────┐ ┌──────┐ │
                    │  ┌────────┐ ┌────────┐       │        │  │  box   │ │ box  │ │
                    │  │  box   │ │  box   │ ...   │        │  │ guest  │ │guest │ │
                    │  │ guest  │ │ guest  │       │        │  └────────┘ └──────┘ │
                    │  └────────┘ └────────┘       │        └──────────┬───────────┘
                    └──────────────┬───────────────┘                   │
                                   └──────── bucket (S3) ──────────────┘
```

Three binaries:

- **`hutch`** (`cmd/hutch`), on your computer: the CLI and the TUI. It keeps no state beyond your config and its SSH key.
- **`hutchd`** (`cmd/hutchd`), on every machine, as a systemd service. On the lead it is the **server**, which keeps the state and answers the CLI. On every machine it is also an **agent** and a **worker**, which run that machine's boxes. A lead's server talks to its own agent in memory, and to other machines' agents over the link.
- **`hutch-guest`** (`cmd/hutch-guest`), PID 1 in every box.

## Connections

**CLI to lead.** The CLI runs `ssh <machine>` and speaks to `hutchd`'s Unix socket at `/run/hutch/api.sock` through it (`internal/client`). Calls are framed JSON over yamux streams (`internal/rpc`, `internal/wire`): each call opens a stream, so a long one, like a template build streaming its output, doesn't block the rest. Access to that socket is administrative access to everything: see the [security model](security.md).

**Lead to workers.** A worker dials the lead on `machine.link_port` with mutual TLS, from a certificate it got by enrolling with a one-time token (`internal/link`, `internal/pki`). The same RPC runs over that link, in both directions.

**Worker to box.** With Firecracker, the worker reaches `hutch-guest` over vsock; with gVisor, over TCP inside the sandbox's network namespace, with a token (`internal/guestproto`). Frames from a guest are untrusted and bounded.

**Box to the internet.** A box's traffic leaves through its own network namespace. HTTPS goes through the **keys proxy** on the box's machine (`internal/keys`, started by each machine's agent), which swaps login placeholders for real credentials toward each agent's declared hosts and adds box identity headers. Signing certificates and identity tokens, and refreshing logins, happen on the lead, which every proxy asks. Private addresses are blocked except granted `network.allow` targets.

## Running a box

The worker (`internal/worker`) runs each box with the machine's runtime. Both start the same disk, `rootfs.ext4`, so a box's files are the same on either.

**Firecracker** (`vm.go`, `internal/firecracker`). Each box is a Firecracker process started by the jailer, in a chroot with hard links to the box's files, its own cgroup, and an unprivileged user. Hutch's guest kernel boots the disk, and the guest is reached over vsock. Every VM is given a CPU template, Hutch's copies of Firecracker's T2A (AMD) and T2CL (Intel) in `internal/firecracker/templates`, so the CPU a guest sees is the same on every machine of a vendor, and memory snapshots move between them. The snapshot class is the vendor, the template, and the Firecracker version.

**gVisor** (`gvisor.go`). Each box is a `runsc` sandbox whose root is the same disk, loop-mounted on the machine while it runs, and the guest is reached over TCP with a token. Its flags (`runscFlags`) must agree on every call for a box: a checkpoint restores only into the sandbox spec it was taken in, so a change to them bumps `specVersion`, which makes a new snapshot class. The class also names the gVisor version, and a restore checks the CPU features the snapshot recorded (`restoresWarm`).

**Networking** (`net.go`). Each box gets a slot: a network namespace with a tap device, NAT for its outbound traffic, and rules that drop private ranges, link-local addresses including cloud metadata, other slots, and the machine itself, except granted `network.allow` targets. Every box sees the same guest addresses, so a restored snapshot's ARP cache stays valid. gVisor's network stack takes over every interface in its namespace, so a gVisor box sits in a second namespace behind its slot's.

**Disks and memory** (`store.go`, `mem.go`, `memory.go`). A fork reflinks the source's disk and shares its memory file copy-on-write, so many boxes start from one template cheaply. A Firecracker snapshot is a diff, applied into the box's sparse memory file. On a machine with memory overcommit, a running VM's balloon reclaims what its guest doesn't use.

## What happens on `hutch new`

1. **CLI** (`cmd/hutch/box.go`): resolves the config and grants, connects over SSH, and calls `BoxFork` with the project, the box's settings, and its logins' grants.
2. **Server** (`internal/server/templates.go`, `forkProject`): takes the project's newest template version, checks the grants against the version's logins, and picks a machine for it (`scheduler.go`), fetching the version from the bucket if that machine doesn't have it.
3. **Worker**: forks the version's snapshot, reflinking its disk and sharing its memory, and restores it warm. The services are already running.
4. **Server**: links the box's checkout to your path, writes your user layer (agent config, placeholders for logins), checks out `hutch/<box>` from the base, and runs `on-create.sh`. A box that fails before it's on its branch is removed.
5. **CLI**: starts the first terminal (`ThreadNew`), which `hutch-guest` runs in tmux, with the agent's command from its adapter.

Most commands follow the same path: the CLI resolves settings and calls one method, the server decides and records, and the worker that holds the box does the work.

## Sleep, checkpoints, and uploads

Each machine's agent (`internal/agent/idle.go`) checks its awake boxes every `idle.check_interval`: CPU use, attached terminals, open forwarded connections. A box idle for its timeout sleeps: the worker snapshots it, memory included, and stops it. An active box is checkpointed every `checkpoint.interval` instead: snapshotted in place, and left running.

Either way, a new generation exists on the machine's disk, and the uploader sends it to the bucket (`store.go`, `Upload`). It reads only the blocks that changed since the last upload, comparing against a reflinked reference copy, and stores them as encrypted, content-addressed chunks (`internal/durable`) named by a manifest per generation. Chunks shared with earlier generations, or with other boxes, are stored once.

Garbage collection (`internal/durable/gc.go`) runs on the lead every `gc.interval`. It lists chunks before reading manifests, so a chunk uploaded for a manifest not written yet is young and spared, never deletes chunks younger than `gc.grace`, and deletes with `If-Match` on the listed ETag, so a chunk an upload refreshed meanwhile survives.

## State

The lead's state is a SQLite database under `/var/lib/hutch/state`, with the machine's master key beside it (`internal/server/store.go`). Secrets in it are sealed with keys derived from the master key.

Boxes live on each machine's XFS disk under `/var/lib/hutch/boxes`: a root filesystem cloned with reflink from its template, plus snapshots. Reflink makes a new box a cheap copy.

The bucket holds everything durable (`internal/bucket`, `internal/durable`): box snapshots and the lead's database, split into encrypted, content-addressed chunks, with manifests naming them. Conditional writes keep two machines from writing the same manifest; conditional deletes keep garbage collection from deleting a chunk an upload just refreshed.

## Packages

| Package | |
| --- | --- |
| `cmd/hutch` | The CLI and TUI: commands, forms, the TUI's list, panes, views, and tasks |
| `cmd/hutchd` | The daemon: `run`, and the steps setup and restore call on the machine |
| `cmd/hutch-guest` | The guest's init and server |
| `internal/server` | The lead: state, scheduling and placement, templates, moves, leads and handovers, the keys proxy's server side, doctor, backups |
| `internal/agent` | A machine's side of the server's calls: idle checks, checkpoints, uploads, memory pressure |
| `internal/worker` | Running boxes: Firecracker VMs and gVisor sandboxes, networking, storage, snapshots |
| `internal/firecracker` | Firecracker's API, and its CPU templates |
| `internal/guest`, `internal/guestproto` | The guest, and the protocol to it |
| `internal/image` | Building images with BuildKit, and the layer Hutch adds to every image |
| `internal/keys` | The keys proxy: placeholders, swaps, refreshes, identity |
| `internal/agents` | Agent adapters: the built-in TOML files, and loading your own |
| `internal/config` | The settings schema, layers, and grants |
| `internal/boxgit` | The Git view's operations on a box's checkout |
| `internal/bucket`, `internal/durable`, `internal/sigv4` | The bucket, encrypted chunks and manifests, and S3 request signing |
| `internal/link`, `internal/pki` | The worker link and its CA |
| `internal/client`, `internal/rpc`, `internal/wire`, `internal/api` | Connecting, calls, framing, and the types shared by all three binaries |
| `internal/mcp` | Model-free MCP readiness checks for `doctor --template` |
| `internal/release`, `internal/version` | Downloading machine files for a release, and comparing versions |
| `internal/compat`, `internal/integration` | Guest protocol compatibility tests, and tests against real machines |
| `images/` | The guest kernel and the static tmux layer |

## Tests

| | Runs | Covers |
| --- | --- | --- |
| `make check` | Anywhere: `go vet`, `go test ./...`, and `govulncheck` | Everything that runs without a machine, including the docs' generated references |
| `make compat` | In a privileged container | The current client against every guest version in `compat/guests.txt`: old boxes must keep working |
| `internal/integration` | Only in CI, on a disposable machine (`HUTCH_TEST_DISPOSABLE`) | Real boxes on each runtime: lifecycle, failures, credentials, upgrades |
| `runtime` workflow | On every push to `main` and pull request, and nightly | Both runtimes, snapshot export and import across Firecracker versions, and upgrades |
| `first-box` workflow | In the `release` workflow, on each tag | A fresh install from the release's artifacts, start to finish |

A change to the guest, snapshots, or networking needs the `runtime` workflow to pass; a change to the CLI, `make check`, and the `clients` job in `ci` for macOS and Windows.

## Where to start

- **A new command:** add it in `cmd/hutch`, with an API method in `internal/api` and its handler in `internal/server`. Commands the TUI can run belong in its palette too (`cmd/hutch/tuiactions.go`). Run `go test ./cmd/hutch -run TestCLIReference -update` to update the [CLI reference](reference/cli.md).
- **A new setting:** add it to the schema in `internal/config/schema.go`, then `go test ./internal/config -run TestSettingsReference -update` for the [settings reference](reference/settings.md).
- **A new agent:** usually only a TOML adapter, like those in `internal/agents/builtin`. See [another agent](agents.md#another-agent).
- **Guest changes:** keep old guests working: boxes and snapshots outlive releases. `make compat` runs the compatibility suite.
