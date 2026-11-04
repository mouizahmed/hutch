# Agents, logins, and MCPs

Hutch runs each agent's own CLI in a box, as you would on your computer. This page covers signing agents in, bringing your agent config and MCP servers into boxes, and adding an agent Hutch doesn't know. Claude Code, Codex, and the GitHub CLI are built in.

## Log in

```sh
hutch login claude gh      # or codex, or any of them
```

Each login runs the tool's own sign-in flow in a temporary box: a browser link or a device code, as usual. Hutch keeps the result on the machine, encrypted, and removes the box. With no names, `hutch login` signs in whatever isn't signed in yet.

Every box gets the logins, but never the real tokens. A box holds placeholders, and a proxy on the machine swaps them for the real credentials only in requests to that agent's own hosts, such as `anthropic.com` for Claude. Refreshes happen on the machine, so one box refreshing a login doesn't sign the others out. See the [security model](security.md#credentials) for what this protects and what it doesn't.

`gh` gives boxes Git access to your GitHub repositories, which private projects need, and the `gh` command.

To keep a login you already made inside a box:

```sh
hutch login --from <box> claude
```

To sign in again, for a different account or after a provider revokes the login, run `hutch login <agent>` again.

### Which boxes get which logins

`credentials` decides which logins a project's boxes get; `["*"]`, the default, means all of them. A project can ask for less in its `hutch.toml`; it can't grant itself more than your own config allows. See [configuration](configuration.md#grants).

## Bring your config

```sh
hutch user sync      # preview, then apply
hutch user status    # what boxes get now
```

Sync copies your agent config from this computer to the machine, and every new box gets it: Claude's `CLAUDE.md`, `settings.json`, skills, agents, commands, and plugins; Codex's `config.toml` and `AGENTS.md`; and your Git name and email. It shows the changes before applying them. Boxes that are already running keep what they had.

Paths under your home directory are rewritten to the box's. Sync warns about anything that can't work in a box, such as a macOS path in a config file. It leaves out an agent's gateway settings when the gateway is on this computer's loopback, like `ANTHROPIC_BASE_URL=http://127.0.0.1:8317`, and boxes use their own login instead.

For tools of your own in every box, put a script at `~/.config/hutch/user-setup.sh`. It travels with sync and runs as `dev` in every template build, after the agents are installed.

## MCP servers

MCP servers configured in your agent's user config come with sync, and run inside the box, natively in the agent. Hutch doesn't need an agent SDK.

- **Remote HTTP servers** need a URL reachable from the box, and their own authentication.
- **Local (stdio) servers** need their command and runtime installed in the box, through the project's Dockerfile or `user-setup.sh`. A command on your computer isn't in the box.

Claude's project-local servers in `~/.claude.json` aren't copied. Use user-scoped servers, or a `.mcp.json` committed to the repository.

### Keys for MCP servers

Keys in an MCP server's `env` and headers are kept with the logins, not the config, and boxes get placeholders for them. For a local server, tell Hutch which HTTPS hosts it sends its key to, so the proxy swaps the placeholder there:

```sh
hutch config set mcp_hosts.github api.github.com
hutch user sync
```

Bare host names mean port 443; `api.example.com:8443` names another. Without known hosts, the server's keys stay real in the box, and sync warns. Plain HTTP, other protocols, and short or non-token secrets also stay real.

The server's HTTP client must honor `HTTPS_PROXY` and trust the machine's CA. Hutch turns on Node 24's proxy support and sets the CA bundle for common Python clients. Older Node versions and libraries that ignore proxy settings need a proxy-aware wrapper.

Some fields are secret but can't be swapped, such as Codex's `oauth.client_secret`: an adapter lists them as `raw_secrets`. They're stored encrypted and sent only to granted boxes, but they're real there, and sync and doctor warn.

### MCP servers with OAuth

An MCP server's own OAuth sign-in is separate from the agent's login. Sign in with the agent inside the box; your computer's keychain and token caches aren't copied. The tokens stay in the box and its snapshots, so copying a signed-in box copies them too, which can break a server that rotates its refresh tokens: sign in separately in boxes that need to be independent.

A local AWS server like `mcp-proxy-for-aws` needs `uvx` and its own AWS sign-in inside the box; your computer's AWS profile doesn't come with it. Don't put long-lived AWS keys in agent config.

### Check

`hutch doctor` checks logins, their placeholders, and MCP key hosts, without waking boxes or contacting providers. `hutch doctor --template` also starts each MCP server in a fresh build and lists its tools. Neither proves a provider still accepts a login: the agent itself is the check.

## Another agent

An adapter is a TOML file describing an agent's CLI. Put yours in `~/.config/hutch/agents/<name>.toml`, then run `hutch user sync`. This example shows the fields for a made-up agent; check each against your agent's own documentation:

```toml
name    = "myagent"
install = "npm install -g myagent@1.4.0"     # runs as dev in template builds
start   = "myagent {prompt}"                  # split on spaces; use a wrapper script for shell syntax
autonomy = { full = ["--yes"], ask = [] }     # flags for each autonomy setting
model   = "--model {model}"
pick    = "myagent sessions"                  # its own picker, for a terminal whose agent exited
login   = "myagent login"

# Where its login lives, and which fields are secret: boxes get placeholders.
credentials = [{ file = "~/.myagent/auth.json", secrets = ["access_token", "refresh_token"] }]
hosts       = ["myagent.dev"]                 # where the proxy may use the real values, subdomains too
token_url   = "https://auth.myagent.dev/oauth/token"   # where its login refreshes: on one of its hosts

config = ["~/.myagent/config.toml"]           # copied by hutch user sync
```

A custom agent runs and signs in like a built-in one. Moving its conversation with `hutch cloud` and `hutch local` needs a transcript format Hutch reads, which today means Claude Code's or Codex's.
