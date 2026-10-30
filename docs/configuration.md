# Configuration

Settings resolve from defaults, the machine, your user config, the project's `.hutch/hutch.toml`, and command flags. The schema rejects unknown keys, incorrect layers, types, and ranges. Each TOML file uses `version = 1`.

```sh
hutch config explain                   # resolved settings, source, and descriptions
hutch config get idle_timeout
hutch config set default_agent codex
hutch config edit                      # ~/.config/hutch/config.toml
hutch config edit --project
hutch config validate
```

`$XDG_CONFIG_HOME/hutch` replaces `~/.config/hutch` when set. `machine` names the SSH target for this computer; it stays local when other settings sync. `HUTCH_MACHINE` overrides it.

## Common settings

| Key | Layer | Default | Purpose |
|---|---|---|---|
| `default_agent` | user or project | `claude` | Agent for new terminals |
| `size.cpu` | project | `4` | Box vCPUs |
| `size.memory` | project | `8G` | Box memory |
| `disk.size` | project | `50G` | Sparse disk capacity |
| `idle_timeout` | project | `10m` | Sleep after inactivity |
| `autonomy` | user or project request | `full` | `ask` or `full` |
| `credentials` | user or project request | `["*"]` | Agent login grants |
| `network.allow` | project request | empty | Explicit private `host:port` targets |
| `network.egress` | user or project request | `open` | Public internet; `allowlist` is refused until implemented |
| `mcp_hosts.<server>` | user | empty | HTTPS hosts a stdio MCP uses its keys toward |
| `keys.identity_hosts` | user | empty | Exact HTTPS hosts receiving a signed box identity |
| `ports.wake_wait` | machine | `30s` | Forwarded connection's wake deadline |
| `ports.parked_max` | machine | `100` | Maximum connections waiting for wake |
| `notify.command` | user | empty | Optional event handler command |

Use `hutch config explain` for the full reference generated from the running version's schema. The per-project grants in your user config bound requests from a repository; a project cannot grant itself more access.

## Agents and MCPs

Built-in adapters describe Claude Code, Codex, and GitHub CLI. Custom adapters live in `~/.config/hutch/agents/*.toml` and travel with `hutch user sync`. An adapter declares its native start, resume, install and login commands, supported files, secret fields, and credential hosts. Start commands are argument lists split on whitespace; use an executable wrapper for shell syntax. Custom agents can launch without Hutch parsing their provider-specific transcripts; session discovery and handoff depend on supported formats.

`hutch user sync` previews changes before applying them. Ordinary config, MCP secret fields, and agent credentials have different handling. Review path warnings: a laptop path or executable is not automatically available inside a Linux box.

MCP clients run natively inside the selected agent; Hutch does not require an agent SDK. Remote HTTP MCPs need reachable URLs and their own authentication. Local stdio MCPs need their runtime and command installed in the box. Only config locations declared by the adapter are collected; unlisted project-local or keychain state does not transfer automatically.

For a stdio MCP called `github` that sends its API key over HTTPS:

```sh
hutch config set mcp_hosts.github api.github.com
hutch user sync
```

The proxy can replace supported token values in HTTPS headers toward configured hosts, including an explicitly configured nonstandard port such as `api.example.com:8443`. Bare host names mean port 443. Without known HTTPS targets, that MCP's keys remain real in the box and sync warns. Plain HTTP cannot use this proxy and keeps real credentials. Non-HTTPS protocols, short or non-token secrets, and project secret files can still contain real credentials. See [security](security.md).

The MCP server's HTTP client must honor `HTTPS_PROXY` and the machine CA. Hutch enables Node 24's environment proxy support and sets the system CA bundle for common Python clients. Older Node versions and libraries that ignore proxy settings need a proxy-aware client or wrapper. Claude's default project-local servers in `~/.claude.json` are outside the built-in adapter's transferred user scope; use user-scoped MCP configuration or a project `.mcp.json` in Git.

`hutch doctor` checks the configured default adapter, captured login fields and their proxy placeholders, and MCP token targets without waking boxes or contacting providers. It warns about missing logins, unsupported token targets, and unverified MCP runtime/authentication. Stored credentials passing this check does not establish that a provider session is still valid. Use `hutch doctor --template` for fresh builds and MCP transport checks, and the native client to verify its authentication.

## Box identity

```sh
hutch config set keys.identity_hosts service.example.com
hutch machine identity > hutch-identity.json
```

Give the output's issuer and public JWKS to your service. Verify EdDSA signatures and `kid`, pin the issuer, require an exact `aud` matching the service host, validate `exp`/`nbf`, and authorize the `sub` (box) and `project` claims. Tokens last five minutes and have unique `jti` values. A service requiring stronger replay protection must enforce it itself.

Hutch adds `X-Hutch-Identity` outside the box, replacing a spoofed header, only for exact configured hosts over HTTPS on port 443. It preserves the service's normal `Authorization`. No extra public machine port is opened. The signing key derives separately from the machine master key and is never written into boxes. Removing a host stops new tokens; already issued tokens remain valid until expiry. Identity proves which box made a request, not that its code or intent is trustworthy.
