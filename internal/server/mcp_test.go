package server

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
)

// A server with hosts gets placeholders; one without keeps its real keys, even a
// key it shares with a server that has hosts, or it would stop working.
func TestMCPFiles(t *testing.T) {
	file := guestproto.File{Path: "~/.claude.json", Data: []byte(`{"mcpServers":{"linear":{"headers":{"Authorization":"Bearer LINEARKEY"}},"local":{"env":{"TOKEN":"SHAREDKEY"}},"api":{"headers":{"X-Key":"SHAREDKEY"}}}}`)}
	logins := []keys.Login{
		{Name: "claude/mcp/linear", Adapter: "claude", Secrets: []keys.Secret{{Real: "Bearer LINEARKEY", Placeholders: []string{"Bearer PH1"}}}},
		{Name: "claude/mcp/api", Adapter: "claude", Secrets: []keys.Secret{{Real: "SHAREDKEY", Placeholders: []string{"PH2"}}}},
	}
	files, err := mcpFiles("claude", []guestproto.File{file}, logins, map[string]bool{"SHAREDKEY": true})
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[0].Data)
	if strings.Contains(got, "LINEARKEY") || !strings.Contains(got, "Bearer PH1") {
		t.Errorf("hosted key not replaced: %s", got)
	}
	if strings.Contains(got, "PH2") || strings.Count(got, "SHAREDKEY") != 2 {
		t.Errorf("a key a hostless server uses was replaced: %s", got)
	}
	if !strings.Contains(string(file.Data), "LINEARKEY") {
		t.Error("the stored file was changed")
	}
}

// Rotating a static MCP token must keep old placeholders usable by running
// stdio processes, whose environment cannot be rewritten by a config sync.
func TestMCPRotationAndTransport(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "test.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s := &Server{store: st}
	a := agents.Builtin()["claude"]
	old := "hutch_old_synthetic_token_1234567890"
	newValue := "hutch_new_synthetic_token_1234567890"
	file := func(value string) guestproto.File {
		return guestproto.File{Path: a.MCP.File, Data: []byte(`{"mcpServers":{"http":{"headers":{"Authorization":"` + value + `"}},"https":{"headers":{"Authorization":"` + value + `"}}}}`)}
	}
	if err := s.syncMCP(a, []guestproto.File{file(old)}); err != nil {
		t.Fatal(err)
	}
	before, err := s.mcpSecrets(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.syncMCP(a, []guestproto.File{file(newValue)}); err != nil {
		t.Fatal(err)
	}
	after, err := s.mcpSecrets(a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"http", "https"} {
		if len(after[name]) != 1 || after[name][0].Real != newValue || after[name][0].Placeholders[0] != before[name][0].Placeholders[0] {
			t.Fatalf("%s rotation dropped the old alias", name)
		}
	}
	layer := api.UserLayer{Files: map[string][]guestproto.File{a.Name: {{Path: a.MCP.File,
		Data: []byte(`{"mcpServers":{"http":{"url":"http://localhost:3000/mcp"},"https":{"url":"https://mcp.example:8443/mcp"}}}`)}}}}
	data, _ := json.Marshal(layer)
	if _, _, err := st.putLayerPart("config", 0, data); err != nil {
		t.Fatal(err)
	}
	logins, real, err := s.mcpLogins(agents.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	if len(logins) != 1 || logins[0].Name != "claude/mcp/https" || !logins[0].ForTarget("mcp.example", "8443") || !real[newValue] {
		t.Fatalf("transport policy: %+v, raw=%v", logins, real)
	}
	// The same value is also needed by plain HTTP, so it must remain real.
	files, err := mcpFiles(a.Name, []guestproto.File{file(newValue)}, logins, real)
	if err != nil {
		t.Fatal(err)
	}
	got := string(files[0].Data)
	if strings.Count(got, newValue) != 2 {
		t.Fatal("plain HTTP's key was replaced by an unusable placeholder")
	}
}

func TestOAuthClientSecretGrantAndBodySemantics(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "oauth.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s := &Server{store: st}
	a := agents.Builtin()["codex"]
	const secret = "synthetic-confidential-client-secret"
	file := guestproto.File{Path: a.MCP.File, Merge: true, Data: []byte("[mcp_servers.fixture.oauth]\nclient_secret = \"" + secret + "\"\n[mcp_servers.fixture.http_headers]\nX-Shared = \"" + secret + "\"\n")}
	data, _ := json.Marshal([]guestproto.File{file})
	if _, _, err := st.putLayerPart(credentialsPrefix+a.Name+"/mcp", 0, data); err != nil {
		t.Fatal(err)
	}
	layer := api.UserLayer{Files: map[string][]guestproto.File{a.Name: {{Path: a.MCP.File, Data: []byte("[mcp_servers.fixture]\nurl=\"https://mcp.example/mcp\"\n")}}}}
	data, _ = json.Marshal(layer)
	if _, _, err := st.putLayerPart("config", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.syncMCP(a, []guestproto.File{file}); err != nil {
		t.Fatal(err)
	}
	denied, err := s.boxFiles([]string{})
	if err != nil || len(denied) != 0 {
		t.Fatal("confidential OAuth configuration reached an ungranted box")
	}
	allowed, err := s.boxFiles([]string{a.Name})
	if err != nil || len(allowed) != 1 || strings.Count(string(allowed[0].Data), secret) != 2 {
		t.Fatal("body credential became an unusable placeholder, including its shared header value")
	}
	checks := s.doctorUser(a.Name)
	for _, c := range checks {
		if c.Name == "MCP keys" && c.Status == "warn" {
			return
		}
	}
	t.Fatal("native OAuth client secret was not reported as remaining real")
}

// A sync cut short between storing the MCP secret files and recording their
// keys must not send the new keys into boxes real.
func TestMCPSyncCutShort(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "test.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s := &Server{store: st, workers: map[string]*workerConn{}}
	a := agents.Builtin()["claude"]
	file := func(v string) []guestproto.File {
		return []guestproto.File{{Path: a.MCP.File, Data: []byte(`{"mcpServers":{"api":{"headers":{"Authorization":"Bearer ` + v + `"}}}}`), Merge: true}}
	}
	layer := api.UserLayer{Files: map[string][]guestproto.File{a.Name: {{Path: a.MCP.File, Data: []byte(`{"mcpServers":{"api":{"url":"https://mcp.example/mcp"}}}`)}}}}
	data, _ := json.Marshal(layer)
	if _, _, err := st.putLayerPart("config", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.syncMCP(a, file("old_synthetic_key_0123456789")); err != nil {
		t.Fatal(err)
	}
	// The new secret files are stored, but syncMCP never ran for them.
	data, _ = json.Marshal(file("new_synthetic_key_0123456789"))
	if _, _, err := st.putLayerPart(credentialsPrefix+a.Name+"/mcp", -1, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.boxFiles([]string{a.Name})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if strings.Contains(string(f.Data), "new_synthetic_key") {
			t.Fatalf("a new MCP key went into the box real: %s", f.Data)
		}
	}
}
