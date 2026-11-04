# Release notes

Each release's notes say what changed, how to upgrade, and the limitations that still apply. Upgrading is the same each time: run the installer again for the new CLI, then `hutch machine upgrade`. See [install](install.md#upgrading).

| Version | Published (UTC) | |
| --- | --- | --- |
| [v0.2.5](releases/v0.2.5.md) | 2026-11-04 | Machine reset, missing-agent errors, accurate worker timestamps, and clearer cloud handoffs |
| [v0.2.4](releases/v0.2.4.md) | 2026-11-04 | Line counts for each box's work in the TUI, a clearer Git view, and fixes found setting up a fresh pair of machines |
| [v0.2.3](releases/v0.2.3.md) | 2026-11-04 | Fixes for backups and canceling commands; garbage collection needs conditional deletes |
| [v0.2.2](releases/v0.2.2.md) | 2026-11-03 | The TUI does what the CLI does: Git, Ports, and Machines views, the action palette, and template guidance |
| [v0.2.1](releases/v0.2.1.md) | 2026-11-03 | Fixes for several machines, and a lead that only coordinates |
| [v0.2.0](releases/v0.2.0.md) | 2026-11-03 | Several machines: a lead and its workers, placement, moves, and recovery |
| [v0.1.0](releases/v0.1.0.md) | 2026-11-01 | The first release: one machine, templates, boxes, and persistent terminals |

## Current limitations

These apply to the current release; each guide says where one affects what you're doing.

- Machines are x86_64 only. The CLI runs on Linux, macOS, and Windows, on amd64 and arm64.
- Outbound internet from boxes is open. Restricting it to a list of domains isn't implemented: boxes refuse to start with `network.egress = "allowlist"`.
- A lead that dies isn't replaced automatically: you promote a worker with `hutch machine lead --from-backup`. See [backups and recovery](backups.md#a-lost-lead).
- On native Windows, `hutch cloud` and `hutch local` move code, but not agent conversations.
- Without a backup bucket, losing a machine loses its boxes.
- Hutch has not had an independent security review. See the [security model](security.md).

Maintainers: see [release preparation](releases.md) for how a release is built, checked, and published.
