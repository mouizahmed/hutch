package main

import (
	"os"
	"path/filepath"
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

func TestScopedMCPWarning(t *testing.T) {
	home := t.TempDir()
	a := agents.Builtin()["claude"]
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"global":{}},"projects":{"/private/project":{"mcpServers":{"local":{"env":{"TOKEN":"do-not-display"}}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	warning := scopedMCPWarning(home, a)
	if warning == "" || strings.Contains(warning, "do-not-display") || strings.Contains(warning, "/private/project") {
		t.Fatalf("missing or unsafe warning: %q", warning)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"global":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warning := scopedMCPWarning(home, a); warning != "" {
		t.Fatalf("user scope should transfer: %q", warning)
	}
}
