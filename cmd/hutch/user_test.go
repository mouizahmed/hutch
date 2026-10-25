package main

import (
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// MCP env values are secrets: they must leave the config part entirely.
func TestSplitSecrets(t *testing.T) {
	for _, f := range []guestproto.File{
		{Path: "~/.claude.json", Data: []byte(`{"mcpServers": {"db": {"command": "pg-mcp", "env": {"PGPASSWORD": "hunter2"}}, "fs": {"command": "fs"}}}`)},
		{Path: "~/.codex/config.toml", Data: []byte("[mcp_servers.db]\ncommand = \"pg-mcp\"\n[mcp_servers.db.env]\nPGPASSWORD = \"hunter2\"\n")},
	} {
		key := "mcpServers"
		if strings.HasSuffix(f.Path, ".toml") {
			key = "mcp_servers"
		}
		cfg, secret, err := splitSecrets(f, key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(cfg.Data), "hunter2") || !strings.Contains(string(cfg.Data), "pg-mcp") {
			t.Errorf("%s config: %s", f.Path, cfg.Data)
		}
		if secret == nil || !secret.Merge || !strings.Contains(string(secret.Data), "hunter2") {
			t.Errorf("%s secret: %+v", f.Path, secret)
		}
	}
}
