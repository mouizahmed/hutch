package agents

import (
	"slices"
	"testing"
)

// autonomy = "ask" must start an agent without its permission-skipping flags.
func TestArgvAutonomy(t *testing.T) {
	s := Builtin()
	full, _ := s.Argv("claude", "", "fix the bug", "full")
	ask, _ := s.Argv("claude", "", "fix the bug", "ask")
	if !slices.Equal(full, []string{"claude", "--dangerously-skip-permissions", "fix the bug"}) {
		t.Errorf("full: %q", full)
	}
	if !slices.Equal(ask, []string{"claude", "fix the bug"}) {
		t.Errorf("ask: %q", ask)
	}
	if _, err := s.Argv("gh", "", "", "full"); err == nil {
		t.Error("gh is a login, not an agent")
	}
}
