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
	got := string(mcpFiles("claude", []guestproto.File{file}, logins, map[string]bool{"SHAREDKEY": true})[0].Data)
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
	got := string(mcpFiles(a.Name, []guestproto.File{file(newValue)}, logins, real)[0].Data)
	if strings.Count(got, newValue) != 2 {
		t.Fatal("plain HTTP's key was replaced by an unusable placeholder")
	}
}
