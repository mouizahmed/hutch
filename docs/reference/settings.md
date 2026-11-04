# Settings reference

Every setting Hutch reads, generated from its schema. See [configuration](../configuration.md) for the layers, and how to set them.

Durations are like `30s`, `10m`, `8h`, or `30d`. Sizes are like `512M`, `8G`, or `1T`. **Layers** are where a setting can be set: `machine` (`hutch config set --machine`, or `--on <machine>`), `user` (your config), and `project` (`.hutch/hutch.toml`). `hutch config explain` shows each setting's value for the current project, and where it comes from.

## Boxes

Each box's size, and when it sleeps.

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `default_agent` | `claude` | user, project | Agent for new threads without `--agent` or `--cmd`. *text.* |
| `size.cpu` | `4` | project | vCPUs of every new box. *whole number, 1 to 64.* |
| `size.memory` | `8G` | project | Memory of every new box. *size, 1G to 512G.* |
| `disk.size` | `50G` | project | Disk of every new box (sparse). *size, 8G to 16T.* |
| `idle_timeout` | `10m` | project | A box sleeps after this long without activity. *duration, 1m to 7d.* |
| `idle.cpu_threshold` | `5%` | project | CPU use of a box's vCPUs above which it counts as active. *percentage, 0% to 100%.* |
| `base` | none | project | Branch new boxes start from; empty means the repo's default branch. *text.* |
| `idle.check_interval` | `15s` | machine | How often the machine checks boxes for activity. *duration, 1s to 5m.* |
| `limits.awake_boxes` | `8` | machine, user | Boxes awake at once; past it, boxes queue (the machine's value caps yours). *whole number, 0 to 10000.* |
| `limits.box_max_awake` | `8h` | machine, user | A box awake this long without a break is put to sleep, even mid-task (the machine's value caps yours). *duration, 1m to 365d.* |

## Access

What boxes may use. A project can only request these: see [grants](../configuration.md#grants).

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `autonomy` | `full` | user, project (a request) | Full: agents run without asking for permission; ask: they ask. *one of `ask`, `full`.* |
| `credentials` | `[*]` | user, project (a request) | Logins boxes get; * means all of them. *list.* |
| `mcp_hosts.*` | none | user | Hosts an MCP server that runs in the box (by its name) uses its keys with: boxes get placeholders for them, swapped toward these hosts only. *list.* |
| `network.allow` | none | project (a request) | Private targets (host:port) boxes may reach. *list.* |
| `network.egress` | `open` | user, project (a request) | Open: any public address; allowlist is reserved and refused until enforcement is implemented. *one of `allowlist`, `open`.* |
| `network.domains` | none | project | Reserved for domain-restricted egress; not enforced in this version. *list.* |
| `keys.refresh_timeout` | `1m` | machine | How long a login refresh upstream, and storing its result, may take; it carries on when the box stops waiting. *duration, 5s to 10m.* |
| `keys.placeholders_kept` | `20` | machine | A refreshed JWT's newest placeholders that stay valid; older ones go, as boxes are written the newest when they wake. *whole number, 1 to 1000.* |
| `keys.refresh_reuse` | `1m` | machine | A box's login refresh this soon after another box's is answered with that one, not refreshed again. *duration, 0s to 1h.* |
| `keys.identity_renew` | `1m` | machine | A box's identity token from the lead is renewed this long before it expires (it lasts five minutes). *duration, 0s to 4m.* |
| `keys.identity_hosts` | none | user | Exact HTTPS hosts that receive X-Hutch-Identity, a signed five-minute JWT naming the calling box and project. *list.* |

## Templates

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `template.build_timeout` | `1h` | project | A template build that runs longer fails. *duration, 1m to 24h.* |
| `template.quiet_timeout` | `2m` | project | How long a snapshot waits for the services in .hutch/services to be up. *duration, 1s to 1h.* |
| `image` | none | project | A published image (like node:22) the template is built from, instead of .hutch/Dockerfile. *text.* |
| `retention.template_versions` | `2` | machine | Template versions kept per project, besides those boxes were forked from. *whole number, 1 to 100.* |

## The machine

Memory, disk, and placement on each machine.

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `machine` | none | user | Your machine's SSH target, like me@hetzner (set by hutch setup or join); kept on each computer, never synced. *text.* |
| `machine.memory_margin` | `10%` | machine | RAM held back for the host. *percentage, 0% to 50%.* |
| `machine.overcommit` | `1` | machine | 1 means every box reserves its full size, and nothing is ever slept or moved for memory; above 1, boxes are placed by measured peak use, with their full sizes allowed up to this multiple of a worker's memory, and boxes may be slept or moved under pressure. *number, 1 to 4.* |
| `machine.memory_high` | `80%` | machine | With overcommit: past this share of the machine's memory, it takes no new boxes, reclaims guest memory, sleeps idle boxes, then moves working ones. *percentage, 50% to 95%.* |
| `machine.memory_critical` | `90%` | machine | With overcommit: past this, with nowhere to move, the least recently active working box sleeps and you're notified. *percentage, 60% to 98%.* |
| `machine.pressure_stall` | `50ms` | machine | With overcommit: when tasks on the machine stall on memory this long within a second, it handles pressure at once instead of at the next check. *duration, 10ms to 1s.* |
| `machine.peak_window` | `1h` | machine | With overcommit: a box counts at its highest measured memory use over this window. *duration, 5m to 7d.* |
| `machine.disk_reserve` | `10%` | machine | Free disk below which the machine takes no new boxes. *percentage, 0% to 50%.* |
| `machine.sandbox_start` | `1G` | machine | On a gVisor machine, where boxes are admitted by the memory they use, what a starting box counts for until its use shows. *size, 128M to 64G.* |
| `machine.max_boxes` | `0` | machine | Most awake boxes on the machine; 0 means only its memory, CPUs, and disk limit it. *whole number, 0 to 10000.* |
| `machine.run_boxes` | `true` | machine | False makes a machine coordinate only: no new boxes are placed or moved there, and doctor skips its box checks. *true or false.* |
| `placement.order` | `fill` | machine | Among machines a new box fits: fill the fullest, keeping others free, or spread to the emptiest. *one of `fill`, `spread`.* |
| `placement.move_queued_after` | `10m` | machine | A box waking on a machine at machine.max_boxes moves to another of its class with room after this; 0s keeps it queued. *duration, 0s to 7d.* |
| `machine.build_cache` | `20G` | machine | Disk the image builder keeps for cached layers. *size, 1G to 16T.* |
| `machine.address` | none | machine | The address other machines dial when this machine leads (default: the SSH host the CLI reaches it by). *text.* |
| `machine.link_port` | `7443` | machine | The worker link's port on the lead; open only once a second machine is added. *whole number, 1 to 65535.* |
| `machine.chunk_cache` | `30%` | machine | Share of the machine's disk for chunks downloaded from the bucket. *percentage, 0% to 90%.* |

## Several machines

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `machines.heartbeat` | `10s` | machine | Keepalive on each worker link. *duration, 1s to 5m.* |
| `machines.unreachable_after` | `30s` | machine | A silent worker gets no new boxes or wakes after this. *duration, 5s to 1h.* |
| `machines.reconnect_max` | `30s` | machine | Longest wait between a worker's reconnect attempts. *duration, 1s to 1h.* |
| `machines.lost_notify_after` | `5m` | machine | You're notified, with hutch machine rm `--lost`, when a worker stays unreachable this long; its boxes are never restored elsewhere without that. *duration, 10s to 7d.* |
| `machines.handover_timeout` | `1m` | machine | How long a planned handover (hutch machine lead) waits for changes under way before it's called off. *duration, 5s to 1h.* |
| `machines.lead_check` | `30s` | machine | How often the lead, and a worker without one, read the lead record in the bucket. *duration, 5s to 1h.* |
| `machines.enroll_ttl` | `15m` | machine | How long a hutch machine add token stays valid. *duration, 1m to 24h.* |

## Backups

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `checkpoint.first_after` | `2m` | machine | A new box takes its first checkpoint after this long active, so it soon has a snapshot to recover from. *duration, 30s to 24h.* |
| `checkpoint.interval` | `15m` | machine | Awake, active boxes are snapshotted to the bucket this often: the most work losing the machine loses. *duration, 1m to 24h.* |
| `archive_after` | `30d` | user | A box asleep this long drops its memory and keeps its disk; waking it is a cold boot. *duration, 1d to 3650d.* |
| `retention.box_checkpoints` | `3` | machine | Snapshots kept in the bucket per box. *whole number, 1 to 100.* |
| `backup.interval` | `1m` | machine | How often the machine backs up its database to the bucket, when it changed. *duration, 10s to 1h.* |
| `backup.warn_after` | `5m` | machine | Hutch doctor warns when changes have waited this long for a backup. *duration, 1m to 24h.* |
| `retention.backups` | `30d` | machine | Backups of the machine's database are kept this long: all from the last hour, then one per hour for a day, then one per day. *duration, 1d to 3650d.* |
| `gc.interval` | `24h` | machine | How often the machine deletes bucket chunks nothing uses. *duration, 1h to 30d.* |
| `gc.grace` | `24h` | machine | Chunks younger than this are never deleted, so a sweep can't race an upload. *duration, 1h to 30d.* |

## Activity and ports

How Hutch tells a working box or terminal from an idle one, and waits for wakes.

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `threads.check_interval` | `1s` | machine | How often the machine reads awake boxes' thread states, between fuller checks. *duration, 250ms to 1m.* |
| `ports.wake_wait` | `30s` | machine | How long a connection to a sleeping box's port waits for it to wake. *duration, 1s to 10m.* |
| `ports.parked_max` | `100` | machine | Connections waiting for boxes to wake, at most, on a machine. *whole number, 1 to 100000.* |
| `threads.echo_window` | `1s` | machine | Output this soon after you type in a thread is your typing echoed, not the thread working. *duration, 0s to 1m.* |
| `threads.working_window` | `5s` | machine | A thread with output this recent counts as working. *duration, 1s to 10m.* |
| `threads.redraw_window` | `2s` | machine | Output this soon after a terminal attaches to a box is its apps redrawing, not work. *duration, 0s to 1m.* |

## Doctor and MCP checks

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `doctor.cleanup_timeout` | `1m` | machine | How long doctor `--template` may take to remove each of its test boxes. *duration, 5s to 1h.* |
| `doctor.fingerprint_timeout` | `5m` | machine | How long doctor `--template` may take to fingerprint a test build. *duration, 10s to 1h.* |
| `doctor.mcp_timeout` | `5m` | machine | How long doctor `--template` may take to check every MCP server. *duration, 10s to 1h.* |
| `mcp.start_timeout` | `1m` | machine, user | How long one MCP server may take to start and answer doctor's check (npx downloads count). *duration, 1s to 30m.* |

## Notifications and the TUI

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `notify.command` | none | user | Command the machine runs on events, with HUTCH_EVENT, HUTCH_BOX, HUTCH_TITLE, HUTCH_MESSAGE set. *text.* |
| `tui.list_width` | `44` | user | The TUI's list width in columns, beside the terminal. *whole number, 24 to 120.* |
| `tui.kept_panes` | `4` | user | Terminals the TUI keeps connected, so returning to one paints at once. *whole number, 1 to 16.* |
| `tui.reconnect` | `2s` | user | How often the TUI tries the machine again after losing it. *duration, 250ms to 1m.* |
| `tui.task_lines` | `2000` | user | Output lines kept for each TUI task; earlier ones are dropped, and the task says so. *whole number, 100 to 100000.* |
| `tui.cancel_grace` | `5s` | user | How long a canceled TUI task, or one running when Hutch quits, has to stop before it's stopped for good. *duration, 1s to 1m.* |
| `tui.diff_limit` | `2M` | user | The most of one diff the TUI's Git view reads; the rest is left out, and it says so. *size, 64K to 64M.* |

## Grants

Written by Hutch when you grant a project more than your own settings; see [grants](../configuration.md#grants).

| Setting | Default | Layers | |
| --- | --- | --- | --- |
| `grants.*.autonomy` | none | user | Granted to this project: full: agents run without asking for permission; ask: they ask. *one of `ask`, `full`.* |
| `grants.*.credentials` | none | user | Granted to this project: logins boxes get; * means all of them. *list.* |
| `grants.*.network.allow` | none | user | Granted to this project: private targets (host:port) boxes may reach. *list.* |
| `grants.*.network.egress` | none | user | Granted to this project: open: any public address; allowlist is reserved and refused until enforcement is implemented. *one of `allowlist`, `open`.* |
