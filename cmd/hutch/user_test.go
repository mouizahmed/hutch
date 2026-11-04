package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// A gateway on this computer's loopback can't be reached from a box: the
// built-in Claude adapter's gateway fields go, the rest of the file stays, and a
// remote gateway is synced as it is.
func TestDropLocalGateway(t *testing.T) {
	var fields []string
	for _, src := range agents.Builtin()["claude"].Config {
		if src.File == "~/.claude/settings.json" {
			fields = src.Gateway
		}
	}
	local := guestproto.File{Path: "~/.claude/settings.json", Data: []byte(`{"model": "opus", "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8317", "ANTHROPIC_AUTH_TOKEN": "tok"}}`)}
	f, note, err := dropLocalGateway(local, fields)
	if err != nil || note == "" || strings.Contains(string(f.Data), "ANTHROPIC") || strings.Contains(string(f.Data), `"env"`) || !strings.Contains(string(f.Data), `"opus"`) {
		t.Fatalf("local gateway: %s, %q, %v", f.Data, note, err)
	}
	remote := guestproto.File{Path: local.Path, Data: []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://gateway.example", "ANTHROPIC_AUTH_TOKEN": "tok"}}`)}
	if f, note, err := dropLocalGateway(remote, fields); err != nil || note != "" || string(f.Data) != string(remote.Data) {
		t.Fatalf("remote gateway changed: %s, %q, %v", f.Data, note, err)
	}
}

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

func TestNestedOAuthSecretIsNotOrdinaryConfig(t *testing.T) {
	f := guestproto.File{Path: "~/.codex/config.toml", Data: []byte("[mcp_servers.api]\nurl=\"https://mcp.example\"\n[mcp_servers.api.oauth]\nclient_id=\"public-client\"\nclient_secret=\"private-client-secret\"\n")}
	plain, secrets, err := splitSecrets(f, "mcp_servers", []string{"oauth.client_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.Data), "private-client-secret") || !strings.Contains(string(plain.Data), "public-client") {
		t.Fatal("OAuth client secret leaked into ordinary config or public client ID was removed")
	}
	if secrets == nil || !secrets.Merge || !strings.Contains(string(secrets.Data), "private-client-secret") {
		t.Fatal("OAuth client secret was not retained in the credential layer")
	}
}

func TestUserSyncDetectsCredentialOnlyChanges(t *testing.T) {
	files := []guestproto.File{{Path: "~/.codex/config.toml", Data: []byte("private-token")}}
	next := collected{secrets: map[string][]guestproto.File{"codex": files}}
	state := api.UserState{Credentials: []api.Credentials{{Part: "codex/mcp", Digest: api.SecretsDigest(files)}}}
	if len(userSyncDiff(state, next)) != 0 {
		t.Fatal("unchanged credentials were reported as changed")
	}
	files[0].Data = []byte("rotated-private-token")
	if got := userSyncDiff(state, next); len(got) != 1 || got[0] != "~ MCP credentials for codex" {
		t.Fatalf("secret-only rotation was ignored or disclosed: %q", got)
	}
	if got := userSyncDiff(state, collected{}); len(got) != 1 || got[0] != "- MCP credentials for codex" {
		t.Fatalf("secret-only removal was ignored: %q", got)
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
