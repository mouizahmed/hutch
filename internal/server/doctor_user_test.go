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

func TestCapturedLoginReadinessRedactsSecrets(t *testing.T) {
	const real = "synthetic-private-token-never-print"
	a := agents.Builtin()["codex"]
	files := []guestproto.File{{Path: "~/.codex/auth.json", Data: []byte(`{"tokens":{"access_token":"` + real + `"}}`)}}
	for _, tt := range []struct {
		name, status string
		files        []guestproto.File
		secrets      []keys.Secret
	}{
		{"valid captured state", "ok", files, []keys.Secret{{Key: "access_token", Real: real, Placeholders: []string{"synthetic-placeholder"}}}},
		{"no fields", "warn", []guestproto.File{{Path: "~/.codex/auth.json", Data: []byte(`{"tokens":{}}`)}}, nil},
		{"no placeholder", "fail", files, []keys.Secret{{Key: "access_token", Real: real}}},
		{"raw placeholder", "fail", files, []keys.Secret{{Key: "access_token", Real: real, Placeholders: []string{real}}}},
		{"stale proxy token", "fail", files, []keys.Secret{{Key: "access_token", Real: "old", Placeholders: []string{"synthetic-placeholder"}}}},
		{"malformed captured JSON", "warn", []guestproto.File{{Path: "~/.codex/auth.json", Data: []byte(`{"access_token":"` + real)}}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := capturedLoginCheck(a, tt.files, tt.secrets)
			if check.Status != tt.status {
				t.Fatalf("status=%s; want %s", check.Status, tt.status)
			}
			output, _ := json.Marshal(check)
			if strings.Contains(string(output), real) || strings.Contains(string(output), "synthetic-placeholder") {
				t.Fatal("doctor disclosed credential values")
			}
		})
	}
}

func TestDoctorUserMissingLoginAndMCPReadiness(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "doctor.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s := &Server{store: st}
	find := func(checks []api.Check, name string) api.Check {
		t.Helper()
		for _, c := range checks {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("missing doctor check %q", name)
		return api.Check{}
	}
	checks := s.doctorUser("claude")
	if find(checks, "login claude").Status != "warn" {
		t.Fatal("missing default login was not reported")
	}
	for _, c := range checks {
		if c.Name == "login codex" || c.Name == "login gh" {
			t.Fatal("unused optional logins produced a warning")
		}
	}
	if find(s.doctorUser("unknown"), "default agent").Status != "fail" {
		t.Fatal("unknown default agent was accepted")
	}
	if find(s.doctorUser("gh"), "default agent").Status != "fail" {
		t.Fatal("login-only adapter was accepted as an agent")
	}
	const real = "synthetic-MCP-token-never-print"
	const config = `{"mcpServers":{"probe":{"command":"local-server"}}}`
	layer := api.UserLayer{Files: map[string][]guestproto.File{"claude": {{Path: "~/.claude.json", Data: []byte(config)}}}}
	data, _ := json.Marshal(layer)
	if _, _, err := st.putLayerPart("config", 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.syncMCP(agents.Builtin()["claude"], []guestproto.File{{Path: "~/.claude.json", Data: []byte(`{"mcpServers":{"probe":{"env":{"TOKEN":"` + real + `"}}}}`)}}); err != nil {
		t.Fatal(err)
	}
	checks = s.doctorUser("claude")
	// Real keys are a warning; servers this offline check can't start aren't,
	// or doctor could never be all green.
	if find(checks, "MCP keys").Status != "warn" || find(checks, "MCP readiness").Status != "ok" {
		t.Fatal("unsupported key targets not reported, or readiness a permanent warning")
	}
	output, _ := json.Marshal(checks)
	if strings.Contains(string(output), real) {
		t.Fatal("doctor disclosed an MCP key")
	}
	if _, _, err := st.putConfig("user", 0, "version = 1\n[mcp_hosts]\nprobe = [\"api.example.com\"]\n"); err != nil {
		t.Fatal(err)
	}
	if find(s.doctorUser("claude"), "MCP keys").Status != "ok" {
		t.Fatal("declared HTTPS targets were not recognized")
	}
}
