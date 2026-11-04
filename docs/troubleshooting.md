# Troubleshooting

Start with doctor. It checks this computer, the machine, the bucket, logins, boxes, and config, and gives a fix for each problem it finds:

```sh
hutch doctor
```

Then, depending on what's wrong:

| | |
| --- | --- |
| `hutch ls`, `hutch status <box>` | What each box and terminal is doing, and why a box is queued |
| `hutch console <box>` | A box's serial console, even when the box itself is broken |
| `hutch events` | Box, terminal, and machine changes as they happen |
| `ssh <machine> journalctl -u hutchd` | The machine's own log |

Each section below gives a symptom, what usually causes it, and what to do.

## Install and connecting

**`hutch: command not found` after installing.** The installer put it in `~/.local/bin`, which isn't on your `PATH`. Add that directory in your shell's profile, or reinstall with `HUTCH_INSTALL_DIR` set to a directory that is. See [install](install.md).

**Doctor says the CLI and `hutchd` versions differ.** Upgrade whichever is older: run the installer again, then `hutch machine upgrade`.

**Commands can't reach the machine.** Hutch connects with your own SSH. Check `ssh <ssh-host>` works without a password prompt, and that `machine` in your config, or `HUTCH_MACHINE`, names it: `hutch config get machine`.

## Setting up a machine

**`sudo`: connect as root, or give the user passwordless sudo.** Setup and upgrades run commands with `sudo -n`. Add a sudoers rule like `ubuntu ALL=(ALL) NOPASSWD:ALL` for the SSH user.

**Architecture: Hutch machines currently need x86_64.** arm64 machines aren't supported yet. An arm64 computer can use an x86_64 machine.

**Kernel: Linux 6.1 or later.** Upgrade the machine's kernel, or use a newer image.

**No apt-get.** On other distributions, install `xfsprogs`, `zstd`, and `curl` yourself, then run setup again.

**Disk: Hutch needs at least 20 GB.** Free space on the machine, or attach a blank disk, which setup offers to format.

**XFS without reflink.** `/var/lib/hutch` must be XFS created with reflink. Reformatting erases it: only do this on a disk with nothing you need, with `mkfs.xfs -m reflink=1`.

**No `/dev/kvm`.** Not an error: boxes run as gVisor sandboxes instead of Firecracker VMs. For Firecracker, use bare metal or an instance type with nested virtualization.

## Projects and templates

**A box can't clone the repository.** The machine clones from `origin`. For a private GitHub repository, run `hutch login gh`. For other hosts, the repository must be reachable from the machine.

**`commit … is not on the remote (push it first)`.** A build clones the commit at your `HEAD` from `origin`. Push it, or let the build do it: it offers when it sees unpushed commits, and `--commit` does it without asking.

**`.hutch/setup.sh is replaced by .hutch/Dockerfile`.** Your recipe is from before Hutch v0.2. Run `hutch init`, which converts it to a Dockerfile, then move the steps that need the repository into `on-create.sh`.

**A build step fails.** The template box is left running. Look around with `hutch template shell`, try the failing command there, fix the recipe, commit, and build again. Steps that need the repository can't be in the Dockerfile, whose context is `.hutch/` only: put them in `on-create.sh`.

**`verify.sh` fails.** It runs as `dev` in `/workspace`, after `on-create.sh`, with nothing else started for it but the project's services. Run it in the template shell to see why.

**`on-create.sh failed; the box is created anyway`.** The box works, but its setup step didn't finish. Run `.hutch/on-create.sh` in it with `hutch exec`, and fix the script for the next box.

**Doctor's template check says fresh builds differ.** Two builds from scratch produced different package versions or files. Pin the versions in your Dockerfile and lockfiles. See [check a recipe](templates.md#check-a-recipe).

## Boxes

**A box stays `queued`.** The machines are full, or `limits.awake_boxes` boxes are already awake. `hutch ls` says which. Sleep or remove a box, raise the limit, add a machine, or make boxes smaller.

**A box was put to sleep while an agent was working.** Either it was awake for `limits.box_max_awake` (8 hours) without a break, or the machine ran short of memory, which happens on gVisor machines, and on Firecracker machines with `machine.overcommit` above 1. You're notified either way. Attach to wake it; a warm wake carries on where it was.

**After a wake, an agent's terminal shows its session picker.** The box booted cold, after a resize, a long sleep, or a move to a different kind of machine, so its programs started again. Pick the conversation to carry on.

**A box doesn't boot, or hangs.** Read its serial console: `hutch console <box>`. With `-i`, you get a root shell on a running box's console.

**An agent exits at once in a box made with `--image`.** Image boxes don't have the agents installed: use `--cmd bash`, or make the box from a project template, which installs them.

## Logins and MCP servers

**An agent says it isn't signed in, or its login stopped working.** Sign it in again: `hutch login <agent>`. Doctor names logins the machine can't use.

**Doctor warns about real logins in boxes.** Those boxes predate the keys proxy, and hold real tokens. Move their work to new boxes and remove them, then sign in again with `hutch login`.

**Every Claude request fails with `ECONNREFUSED` in boxes.** Your Claude settings point at a gateway on your own computer, like `ANTHROPIC_BASE_URL=http://127.0.0.1:8317`. Sync from Hutch v0.2.4 leaves those settings out; run `hutch user sync` again, and make new boxes.

**An MCP server doesn't start in a box.** Its command isn't installed there: a local server needs its runtime and command in the project's Dockerfile or your `user-setup.sh`. `hutch doctor --template` starts each server in a fresh build and shows the error.

**An MCP server's key doesn't work.** Boxes get placeholders, swapped for the real key only toward the hosts in `mcp_hosts.<server>`. Set those, run `hutch user sync`, and check the server's HTTP client honors `HTTPS_PROXY`. See [keys for MCP servers](agents.md#keys-for-mcp-servers).

## Ports and editors

**`can't forward …: address already in use`.** Something on your computer already uses that port. Forward to another: `hutch forward <box> 3000:3001`.

**A forwarded service doesn't answer after the box slept.** After a cold boot, only services in `.hutch/services` start on their own. Start your dev server again, or add it there.

**`ssh <box>.hutch` asks for a password or fails.** Run `hutch ssh-config` again: it updates the entry with the current path of `hutch` and its key.

## Several machines

**`can't reach the lead at …:7443`.** The worker can't open the link port on the lead. Allow TCP 7443 into the lead from each worker, in the cloud firewall or security group and in the lead's own firewall. See [when a worker can't reach the lead](machines.md#when-a-worker-cant-reach-the-lead).

**A machine shows as offline.** Check it's running and `ssh` reaches it, then `sudo systemctl status hutchd` there. Just after the lead restarts, workers show offline for a few seconds while they reconnect.

**A machine is gone for good.** See [backups and recovery](backups.md).

## Backups

**Doctor: no bucket.** Losing the machine loses its boxes. Add one: `hutch machine bucket`.

**`the bucket doesn't support conditional writes`.** Hutch needs `If-None-Match` on writes, so two machines can't overwrite each other's snapshots. Use a provider that supports it, like AWS S3.

**`hutch machine gc`: the bucket must support conditional deletes.** Garbage collection deletes nothing until it does; backups and restores still work. Use a provider that supports `If-Match` on delete, like AWS S3.

**Doctor warns the backup is behind.** Changes have waited more than `backup.warn_after` to reach the bucket. Look for bucket errors in `journalctl -u hutchd` on the lead: expired keys, a full bucket, or no network.

## Still stuck

Open an issue at [github.com/mouizahmed/hutch](https://github.com/mouizahmed/hutch/issues) with what you ran, what happened, and the output of `hutch doctor`. Report security problems privately, through the [security policy](../SECURITY.md).
