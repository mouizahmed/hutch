package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
)

func TestBackupWarning(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, tt := range []struct {
		name              string
		changes, backedUp int64
		at                time.Time
		want              string
	}{
		{"unchanged", 3, 3, time.Time{}, ""},
		{"never backed up", 3, 0, time.Time{}, "no successful backup"},
		{"recent", 3, 2, now.Add(-time.Minute), ""},
		{"stale", 3, 2, now.Add(-10 * time.Minute), "changes since the last backup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := backupWarning(tt.changes, tt.backedUp, tt.at, now, 5*time.Minute)
			if tt.want == "" && got != "" || tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("got %q; want %q", got, tt.want)
			}
		})
	}
}

func TestHelloAcceptsOnlyCurrentAndPreviousClients(t *testing.T) {
	for _, protocol := range []int{api.Protocol - 2, api.Protocol - 1, api.Protocol, api.Protocol + 1} {
		params, err := json.Marshal(api.Handshake{Protocol: protocol, Version: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := (&Server{}).hello(t.Context(), params, nil)
		if protocol < api.Protocol-1 {
			if err == nil || !strings.Contains(err.Error(), "upgrade hutch on this computer") {
				t.Fatalf("old protocol %d accepted: %v", protocol, err)
			}
			continue
		}
		if err != nil || result.(api.Handshake).Protocol != api.Protocol {
			t.Fatalf("compatible protocol %d refused: %v", protocol, err)
		}
	}
}
