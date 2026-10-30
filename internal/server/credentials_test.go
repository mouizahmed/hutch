package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
)

// A proxy controls new requests; it cannot take raw secrets out of a source
// snapshot. This also covers missing provenance and wildcard grants.
func TestInheritedLogins(t *testing.T) {
	for _, tt := range []struct {
		name       string
		had, creds []string
		allowed    bool
	}{
		{"same", []string{"claude", "gh"}, []string{"gh", "claude"}, true},
		{"broader", []string{"gh"}, []string{"gh", "claude"}, true},
		{"narrower", []string{"gh", "claude"}, []string{"gh"}, false},
		{"wildcard narrowed", []string{"*"}, []string{"gh"}, false},
		{"all", []string{"*"}, []string{"*"}, true},
		{"none", []string{}, []string{}, true},
		{"unknown", nil, []string{}, false},
		{"unknown broad", nil, []string{"*"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := inheritedLogins(tt.had, tt.creds)
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v err=%v", tt.allowed, err)
			}
		})
	}
}

func TestCaptureRefusesManagedPlaceholdersAndMetadata(t *testing.T) {
	const real = "synthetic-provider-token-never-print"
	const placeholder = "synthetic-placeholder-never-print"
	const oldPlaceholder = "synthetic-old-placeholder-never-print"
	known := []keys.Secret{{Key: "access_token", Real: real, Placeholders: []string{placeholder, oldPlaceholder}}}
	a := agents.Builtin()["codex"]
	for _, tt := range []struct {
		name, token, want string
	}{
		{"fresh real login", real, ""},
		{"current placeholder", placeholder, "managed login"},
		{"older placeholder", oldPlaceholder, "managed login"},
		{"metadata without a token", "", "no token fields"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"account": "fixture", "tokens": map[string]string{"access_token": tt.token}})
			err := checkCapturedLogin(a, []guestproto.File{{Path: "~/.codex/auth.json", Data: data}}, known)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
			for _, value := range []string{real, placeholder, oldPlaceholder} {
				if strings.Contains(err.Error(), value) {
					t.Fatal("capture error disclosed a credential value")
				}
			}
		})
	}
}
