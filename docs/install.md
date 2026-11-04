# Install and requirements

Hutch has two parts: the `hutch` command on your computer, and `hutchd` on the Linux machine that runs your boxes. You install the command; `hutch setup` installs the daemon over SSH, matched to the command's version.

## Your computer

The CLI runs on Linux, macOS, and Windows, on amd64 and arm64. It needs Git and an OpenSSH client.

### Linux and macOS

```sh
curl -fsSL https://github.com/mouizahmed/hutch/releases/latest/download/install.sh | sh
```

The script downloads the latest release for your system, checks its SHA-256 against the release's `checksums.txt`, and installs `hutch` into `~/.local/bin` without sudo. Nothing is installed if the checksum doesn't match. If that directory isn't on your `PATH`, the script says so; add it to your shell's profile.

Two variables change what it installs:

```sh
curl -fsSL https://github.com/mouizahmed/hutch/releases/latest/download/install.sh \
  | HUTCH_VERSION=v0.2.4 sh
curl -fsSL https://github.com/mouizahmed/hutch/releases/latest/download/install.sh \
  | HUTCH_INSTALL_DIR=/usr/local/bin sh
```

`HUTCH_VERSION` picks a published version; `HUTCH_INSTALL_DIR` picks the directory, which you must be able to write to.

### Windows

Download `hutch_<version>_windows_amd64.tar.gz` (or `_arm64`) from the [latest release](https://github.com/mouizahmed/hutch/releases/latest), check it against `checksums.txt`, and extract `hutch.exe` somewhere on your `PATH`:

```powershell
(Get-FileHash hutch_v0.2.4_windows_amd64.tar.gz).Hash.ToLower()   # compare with checksums.txt
tar -xzf hutch_v0.2.4_windows_amd64.tar.gz hutch.exe
```

Install Git for Windows and the OpenSSH client (an optional Windows feature). Native Windows has one limit: `hutch cloud` and `hutch local` move code between your computer and a box, but agent conversations stay on the side they started on.

To avoid that limit, use WSL and the Linux installer above.

### Check it

```sh
hutch --version
```

To check where a release came from, beyond its checksum, see [release provenance](releases.md#artifacts).

## The machine

Boxes run on a Linux machine you reach over SSH: a VPS, a cloud instance, or a computer of your own.

| Need | Why |
| --- | --- |
| x86_64 | arm64 machines aren't supported yet; arm64 computers can use an x86_64 machine. |
| Linux 6.1 or newer, with systemd | `hutchd` runs as a systemd service. |
| `ssh <ssh-host>` with a key, and passwordless sudo | Setup and upgrades run over SSH. |
| A blank disk, or at least 20 GB free for an image file | `/var/lib/hutch` must be XFS with reflink; setup formats a blank disk or creates an image file, and asks first. |
| About 8 GB of memory to start | Each awake box reserves its size; leave room for your projects and their snapshots. |

An Ubuntu machine with `apt` is the simplest path.

### Firecracker or gVisor

Setup picks the runtime from the machine. With `/dev/kvm`, boxes are Firecracker microVMs; without it, they are gVisor sandboxes. Most cloud instances don't expose KVM unless they offer nested virtualization or are bare metal. The two isolate boxes differently and support different workloads; see the [security model](security.md).

A machine that only coordinates others needs none of the box requirements; see [several machines](machines.md).

### A backup bucket

Setup offers to back the machine up to an S3-compatible bucket. Without one, nothing survives losing the machine. The bucket must support conditional writes (`If-None-Match`) and conditional deletes (`If-Match`); AWS S3 does. Keep the recovery passphrase somewhere you can reach without the machine.

## Upgrading

Run the installer again for a new CLI, then upgrade the machines to match:

```sh
hutch machine upgrade
```

It upgrades the lead, then every worker. Running boxes keep running.

## Building from source

To build the CLI and daemon yourself, see [contributing](../CONTRIBUTING.md).

Next: [your first box](quickstart.md).
