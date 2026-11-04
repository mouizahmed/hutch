# Several machines

One machine leads: it keeps the state, places boxes, backs up, and answers the CLI. Other machines run boxes as its workers. Every machine runs `hutchd`; your computer talks only to the lead, over SSH.

## Add a machine

The lead needs a backup bucket (`hutch machine bucket`), since workers write their boxes' snapshots to it. Then, for a machine set up like the first ([prerequisites](quickstart.md#prepare-the-machine)):

```sh
hutch machine add <ssh-host>
hutch machine
```

The new machine dials the lead over mutual TLS on port 7443 (`machine.link_port`), so only the lead needs that port open to the others; a worker needs no inbound port. If the lead's address isn't its SSH host, set `machine.address`.

Machines are named by the SSH host you set them up or add them as, like `hutch-vps2`; for an IP address, by their hostname. `--name` chooses another for an added machine.

### When a worker can't reach the lead

`hutch machine add` installs the worker, then waits for it to connect. If it can't, it says so: the lead's port 7443 isn't reachable from the worker.

- **Cloud firewalls and security groups:** allow TCP 7443 into the lead from each worker's address. Workers need no inbound rule.
- **The lead's own firewall:** `ufw`, `firewalld`, or `nftables` on the lead must allow it too, like `sudo ufw allow 7443/tcp`.
- **The address:** workers dial the lead's SSH host. If that's an alias or an address the workers can't use, set the one they can before adding them: `hutch config set --machine machine.address <host>`. Machines in one private network usually want the private address.

`hutch doctor` tries the link port from each worker over SSH, so a blocked port shows as such. A worker's own log is `journalctl -u hutchd` on it.

## Placing boxes

A new box goes to a machine that fits it, preferring one that already has its template version. `placement.order` chooses between filling the fullest machine (`fill`, the default) and spreading to the emptiest (`spread`).

```sh
hutch new --on <machine>                         # place it there, or queue there
hutch move <box> --to <machine>                  # warm within a snapshot class, else cold
hutch config set --on <machine> machine.max_boxes 4
hutch config set --on <machine> machine.run_boxes false   # coordinate only
```

A lead that only coordinates needn't meet the box requirements (KVM, kernel 6.1, XFS): set it up with `hutch setup <ssh-host> --no-boxes`, then add machines that run the boxes.

Machine-layer keys set with `--machine` apply to every machine; `--on` overrides one. A box waking on a machine at `machine.max_boxes` moves to another of its class after `placement.move_queued_after`. `hutch ls` shows each box's machine.

## Memory

On a Firecracker machine, by default, every awake box reserves its full size, so a machine runs only as many boxes as fit in its memory, less `machine.memory_margin` for the machine itself. Boxes that don't fit queue until one sleeps. A gVisor box takes memory only as it uses it, so a gVisor machine admits boxes by their use, counting a starting box as `machine.sandbox_start` (1 GB) until its use shows, and always handles memory pressure as below.

On a Firecracker machine, with `machine.overcommit` above 1, boxes are placed by their measured peak use instead, so more fit: `1.5` lets a machine run boxes whose sizes add up to one and a half times its memory. In return, under memory pressure the machine sleeps idle boxes, moves working ones to another machine, and as a last resort sleeps the least recently active working box and notifies you. `machine.memory_high`, `machine.memory_critical`, and `machine.peak_window` tune when; see the [settings reference](reference/settings.md).

## Logins on workers

The keys proxy's CA key, the identity signing key, and the master key stay on the lead. A worker gets only the logins granted to the boxes it runs, held in memory, and certificates and identity tokens the lead signs. Token refreshes go through the lead, one at a time per login. The bucket's credentials are on every machine; turn on your provider's object versioning or retention to recover from a compromised machine deleting objects.

## Use the machine from another computer

Your setup lives on the machine, so a second computer only needs to be let in:

```sh
hutch machine join <ssh-host>          # on the new computer
```

If the new computer can't SSH in yet, `join` prints its public key. On a computer that already works, let that key in, which also prints the command to run on the new one:

```sh
hutch machine add-computer "<public key>"
```

Your user config [syncs between the two](configuration.md#your-config-on-several-computers).

## When the lead is unreachable

Boxes keep running. Pointed at a worker (`HUTCH_MACHINE=<worker>`), the CLI can list, attach to, exec in, forward ports of, sleep, and wake that worker's boxes until the lead is back. Anything else waits for the lead.

## Removing and losing machines

```sh
hutch machine rm <machine>          # moves its boxes to the others, then removes it
hutch machine rm --lost <machine>   # it's gone: its boxes come back elsewhere from the bucket
```

The first moves every box off the machine, warm where it can, then removes it; it's refused from then on. The second is for a machine that's gone, and is covered in [backups and recovery](backups.md#a-lost-worker). In short: `--lost` first fences the machine over SSH, so it stops running its copies. When it can't be reached, add `--down` only if you know it's off or cut off from the bucket and the other machines: otherwise two copies of its boxes keep working. Every machine writes a heartbeat to the bucket each `machines.lead_check`, and `--down` is refused while a machine's is recent. Boxes come back from their newest snapshot (`checkpoint.interval`); work since then is lost. Hutch never restores them on its own; it notifies you after `machines.lost_notify_after`. A lost machine added back keeps its stale copies asleep as `<box>-returned`.

## Changing the lead

```sh
hutch machine lead <machine>                      # planned: nothing is lost
hutch machine lead <ssh-host> --from-backup       # the lead is dead
```

A planned handover pauses changes for seconds, waits for login refreshes in flight, and hands the database and master key to the other machine; the old lead becomes its worker. Promoting from the backup is a disaster procedure; see [backups and recovery](backups.md#a-lost-lead). It asks for the recovery passphrase, fences the old lead (or needs `--down`), and loses up to `backup.interval` of changes; boxes made or moved in that window are recovered from the workers' reports. Leads are numbered by term in a lead record signed in the bucket; a lead that finds a higher term stops leading and keeps its old state aside until `hutch machine forget-stale <ssh-host>`. Add it back with `hutch machine add`.

## Upgrades and checks

```sh
hutch machine upgrade    # the lead, then every worker
hutch doctor             # each machine's checks, its link, and its view of the bucket
```

Install the new CLI first: an upgrade installs the CLI's own version on every machine. Running boxes are checkpointed and keep running, and each worker reconnects within seconds. Workers already on the new version are skipped, and one that can't be reached over SSH is named. When a version changes the bucket's format, which older versions can't read, the upgrade asks once before the lead is upgraded.
