package main

import (
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// MCP env values and auth headers are secrets: with the built-in adapters'
// declarations they must leave the config part entirely.
func TestSplitSecrets(t *testing.T) {
	set := agents.Builtin()
	for agent, f := range map[string]guestproto.File{
		"claude": {Path: "~/.claude.json", Data: []byte(`{"mcpServers": {"db": {"command": "pg-mcp", "env": {"PGPASSWORD": "hunter2"}}, "api": {"type": "http", "url": "https://mcp.example", "headers": {"Authorization": "Bearer s3cret"}}, "fs": {"command": "fs"}}}`)},
		"codex":  {Path: "~/.codex/config.toml", Data: []byte("[mcp_servers.db]\ncommand = \"pg-mcp\"\n[mcp_servers.db.env]\nPGPASSWORD = \"hunter2\"\n[mcp_servers.api]\nurl = \"https://mcp.example\"\n[mcp_servers.api.http_headers]\nAuthorization = \"Bearer s3cret\"\n")},
	} {
		mcp := set[agent].MCP
		cfg, secret, err := splitSecrets(f, mcp.Key, mcp.Secrets)
		if err != nil {
			t.Fatal(err)
		}
		c := string(cfg.Data)
		if strings.Contains(c, "hunter2") || strings.Contains(c, "s3cret") || !strings.Contains(c, "pg-mcp") || !strings.Contains(c, "mcp.example") {
			t.Errorf("%s config: %s", agent, c)
		}
		if secret == nil || !secret.Merge || !strings.Contains(string(secret.Data), "hunter2") || !strings.Contains(string(secret.Data), "s3cret") {
			t.Errorf("%s secret: %+v", agent, secret)
		}
	}
}
