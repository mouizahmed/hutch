package agents

import (
	"slices"
	"testing"
)

// autonomy = "ask" must start an agent without its permission-skipping flags.
func TestArgvAutonomy(t *testing.T) {
	s := Builtin()
	full, _ := s.Argv("codex", "", "fix the bug", "full", "")
	ask, _ := s.Argv("codex", "", "fix the bug", "ask", "")
	if !slices.Equal(full, []string{"codex", "--dangerously-bypass-approvals-and-sandbox", "fix the bug"}) {
		t.Errorf("full: %q", full)
	}
	if !slices.Equal(ask, []string{"codex", "fix the bug"}) {
		t.Errorf("ask: %q", ask)
	}
	resume, _ := s.ResumeArgv("claude", "ask", "abc")
	if !slices.Equal(resume, []string{"claude", "--resume", "abc"}) {
		t.Errorf("resume: %q", resume)
	}
	if _, err := s.Argv("gh", "", "", "full", ""); err == nil {
		t.Error("gh is a login, not an agent")
	}
}
