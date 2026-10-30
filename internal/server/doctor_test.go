package server

import (
	"strings"
	"testing"
	"time"
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
