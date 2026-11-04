# Ports, SSH, and editors

Boxes have no public address. You reach a box's services and shell through Hutch, over the same SSH connection the CLI uses, so nothing is exposed to the internet. This page covers forwarding ports, SSH and editors, and what boxes themselves can reach.

## Forward a port

For a dev server on port 3000 in a box:

```sh
hutch forward <box> 3000          # localhost:3000 on your computer
hutch forward <box> 3000:8080     # localhost:8080 instead
hutch forward <box>               # every port it listens on, as each one starts
```

Each forward is a listener on your computer's loopback, and lasts until you press Ctrl-C. If a local port is taken, that forward fails; give another with `<port>:<local port>`. With no ports, Hutch watches the box and forwards each new port at the same number, trying again when a local port frees up.

In the TUI, `o` opens the Ports view: what the box listens on, and Enter to forward one, choosing another local port when that one's taken.

### Sleeping boxes

A forward to a named port keeps working while the box sleeps. The first connection wakes it, and waits up to `ports.wake_wait` (30 seconds) for it. An open connection keeps the box awake.

After a warm wake, the service is still running. After a cold boot, it starts again only if it's in the project's [`.hutch/services`](templates.md#the-recipe); a dev server you started by hand needs starting again, and your client may need to retry while it comes up.

## SSH

```sh
hutch ssh-config
```

This adds one entry to `~/.ssh/config` for every box, now and later:

```text
Host *.hutch
  User dev
  ProxyCommand "/path/to/hutch" ssh-proxy %h
  ...
```

Then `ssh <box>.hutch` opens a shell in the box as `dev`, waking it if it sleeps; `scp` and `rsync` work the same way. Connections go through Hutch to the box, authenticated with Hutch's own key, so you don't manage keys or host keys for boxes. `--print` shows the entry without writing it.

## Editors

Editors that connect over SSH use the same entry: in VS Code's Remote-SSH, or JetBrains Gateway, connect to `<box>.hutch` and open `/workspace`. The editor's server runs inside the box, on its CPU and memory, and an open editor counts as activity, keeping the box awake.

These editor setups haven't been through Hutch's release tests; `ssh <box>.hutch` has.

## What a box can reach

- **The internet:** outbound connections to public addresses are open, so agents can install packages and call APIs. A box can also send out anything it can read.
- **Private networks:** blocked, including the machine itself, other boxes, and cloud metadata addresses, except `host:port` targets you allow with `network.allow`. A project can ask for targets in its `hutch.toml`, which you grant; see [grants](configuration.md#grants).
- **Inbound:** nothing, except through forwards and SSH as above.

Restricting outbound traffic to a list of domains isn't implemented yet: with `network.egress = "allowlist"`, boxes refuse to start rather than run open. See the [security model](security.md).
