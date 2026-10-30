package server

import (
	"strings"
	"testing"

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
