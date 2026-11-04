# Configuration

Hutch's settings live in TOML files at a few layers, and each setting resolves from the most specific layer that sets it. This page covers the layers, the `hutch config` commands, and what a project may ask for. Every setting is listed in the [settings reference](reference/settings.md).

## Layers

| Layer | File | Holds |
| --- | --- | --- |
| Defaults | built in | A value for every setting |
| Machine | on the machine; `hutch config set --machine` | Settings for every machine, like `placement.order` |
| One machine | on the machine; `--on <machine>` | One machine's own value, over every machine's |
| User | `~/.config/hutch/config.toml` | Yours, like `default_agent`, on every computer you use |
| Project | `.hutch/hutch.toml` in the repository | The project's box size, idle timeout, and requests |
| Flags | the command line | One command's value, like `hutch new --agent codex` |

Each file starts with `version = 1`. The schema rejects unknown keys, a setting at a layer that can't hold it, wrong types, and out-of-range values. `$XDG_CONFIG_HOME/hutch` replaces `~/.config/hutch` when set.

## Commands

```sh
hutch config explain                   # every setting for this project, and where it comes from
hutch config get idle_timeout
hutch config set default_agent codex   # your config; --project, --machine, or --on <machine> for others
hutch config unset default_agent       # back to the layers below
hutch config edit                      # your config in $EDITOR; --project opens the project form
hutch config validate                  # check every layer against the schema
```

## Common settings

| Key | Layer | Default | |
| --- | --- | --- | --- |
| `default_agent` | user, project | `claude` | Agent for new terminals |
| `size.cpu`, `size.memory` | project | `4`, `8G` | Each box's vCPUs and memory |
| `disk.size` | project | `50G` | Each box's disk, allocated as it's used |
| `idle_timeout` | project | `10m` | A box sleeps after this long without activity |
| `base` | project | the default branch | Branch new boxes start from |
| `autonomy` | user, project request | `full` | `full`: agents run without asking permission; `ask`: they ask |
| `credentials` | user, project request | `["*"]` | Logins boxes get |
| `network.allow` | project request | none | Private `host:port` targets boxes may reach |
| `network.egress` | user, project request | `open` | Public internet; `allowlist` isn't enforced yet, so boxes refuse to start with it |
| `limits.awake_boxes` | machine, user | `8` | Boxes awake at once; more queue |
| `notify.command` | user | none | A command run on events, like an agent waiting for you |

The [settings reference](reference/settings.md) has the rest, including the machine's memory, placement, backup, and TUI settings.

## Your config on several computers

Your user config lives on the machine as well as on each computer, and every `hutch` command syncs the two first, so a setting you change on one computer reaches the others. The one exception is `machine`, the SSH target, which stays on each computer; `HUTCH_MACHINE` overrides it for one command.

If both sides changed since they last matched, the command stops and says so. Choose one:

```sh
hutch config sync --mine     # this computer's copy wins
hutch config sync --theirs   # the machine's copy wins
```

## Grants

A repository is code you may not have written, so its `hutch.toml` can only *request* the settings that give boxes access: `autonomy`, `credentials`, `network.allow`, and `network.egress`. A project gets what it asks for up to your own value, plus whatever you've granted that project.

When a project asks for more, Hutch shows the difference before making a box, and asks whether to grant it. A grant is written to your user config, under `grants.<project>`, and applies to that project only. Without one, the box is made with what's already granted.

```sh
hutch config grants                    # what you've granted each project
hutch config grants <project> --revoke
```

## Notifications

`notify.command` runs on the machine when something needs you. `HUTCH_EVENT` says what: `bell` (an agent wants you), `exited` (an agent exited), `crashed` (a box was lost, or rolled back to its last snapshot), `queued`, `force-slept` (a box slept for memory or `limits.box_max_awake`), `moved`, `returned` (a machine came back with an old copy of a box, kept aside), `unreachable` (a machine), `lead` (a machine stopped leading), or `login` (a login needs signing in again). It also gets `HUTCH_BOX`, `HUTCH_TITLE`, and `HUTCH_MESSAGE`. For example, with [ntfy](https://ntfy.sh):

```sh
hutch config set notify.command 'curl -s -d "$HUTCH_TITLE: $HUTCH_MESSAGE" ntfy.sh/my-hutch-topic'
```

## Moved

Agent logins, `hutch user sync`, MCP servers, and custom adapters are in [agents, logins, and MCPs](agents.md). Box identity is in the [security model](security.md#box-identity).
