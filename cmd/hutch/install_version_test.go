package main

import (
	"testing"

	"github.com/mouizahmed/hutch/internal/version"
)

func TestDowngradeChecksInstalledAndRunningDaemon(t *testing.T) {
	previous := version.Version
	version.Version = "v0.1.0-rc.2"
	t.Cleanup(func() { version.Version = previous })
	for _, c := range []struct {
		name, installed, running string
		force, refused           bool
	}{
		{"newer installed", "v0.1.0-rc.10", "v0.1.0-rc.1", false, true},
		{"interrupted downgrade", "v0.1.0-rc.2", "v0.1.0-rc.10", false, true},
		{"older running", "v0.1.0-rc.2", "v0.1.0-rc.1", false, false},
		{"first setup", "", "", false, false},
		{"explicit force", "v0.1.0-rc.10", "v0.2.0", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := probe{values: map[string]string{"hutchd": c.installed, "hutchd_running": c.running}}
			if err := checkDowngrade(p, c.force); (err != nil) != c.refused {
				t.Fatalf("refused=%v, want %v: %v", err != nil, c.refused, err)
			}
		})
	}
}
