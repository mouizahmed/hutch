# Compatibility checks

`make compat` checks the host client against historical guest protocols in a disposable privileged container. Every released guest is listed in `guests.txt`.

`node compat/mcp-smoke.mjs` exercises native Codex and Claude MCP clients against local fixtures: stdio and authenticated HTTP, discovery and harmless echo calls, then bad credentials, bad paths, and missing commands. It uses a temporary home, removes provider API keys from its environment, and never submits a model turn. Codex's experimental app-server protocol is used only by this diagnostic; Hutch does not depend on it to launch agents. The current CI versions are Codex 0.166.0 and Claude Code 2.1.314.

Both scripts read Hutch's built-in adapters: configs use each adapter's declared MCP header field, and native MCP OAuth tokens must stay out of the files an adapter declares as its login. An agent update that breaks either fails CI.

These checks do not prove third-party MCP OAuth flows, every HTTP library's proxy support, or VM isolation. Runtime checks need both actual box backends; successful native MCP fixtures and guest wire compatibility are separate evidence.
