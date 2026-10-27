package agents

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Sessions are found by the directory they ran in, and moved to another directory
// with the history's paths, but not a sibling directory's, rewritten.
func TestSessionHandoff(t *testing.T) {
	home := t.TempDir()
	write := func(rel, data string, age time.Duration) {
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(data), 0o600)
		at := time.Now().Add(-age)
		os.Chtimes(p, at, at)
	}
	write(".claude/projects/-home-me-app/old.jsonl", "{}\n", time.Hour)
	write(".claude/projects/-home-me-app/new.jsonl", "{}\n", time.Minute)
	write(".claude/projects/-home-me-app/new/subagents/a.jsonl", "{}\n", time.Minute)
	write(".claude/projects/-home-me-app-2/newest.jsonl", "{}\n", 0)
	write(".codex/sessions/2026/10/27/rollout-1-main.jsonl", `{"type":"session_meta","payload":{"id":"main","cwd":"/home/me/app"}}`+"\n", time.Minute)
	write(".codex/sessions/2026/10/27/rollout-2-sub.jsonl", `{"type":"session_meta","payload":{"id":"sub","cwd":"/home/me/app","parent_thread_id":"main"}}`+"\n", 0)

	s := Builtin()
	c, _ := LatestSession(s["claude"], home, "/home/me/app")
	if c == nil || c.ID != "new" || len(c.Files) != 2 {
		t.Fatalf("claude: %+v", c)
	}
	x, _ := LatestSession(s["codex"], home, "/home/me/app")
	if x == nil || x.ID != "main" {
		t.Fatalf("codex: %+v", x)
	}

	data := []byte(`{"cwd":"/home/me/app","file":"/home/me/app/main.go","other":"/home/me/app-2/x"}`)
	rel, out := Relocate("claude", ".claude/projects/-home-me-app/new.jsonl", data, "/home/me/app", "/home/me/app/.hutch/worktrees/b")
	if rel != ".claude/projects/-home-me-app--hutch-worktrees-b/new.jsonl" {
		t.Errorf("rel %s", rel)
	}
	want := `{"cwd":"/home/me/app/.hutch/worktrees/b","file":"/home/me/app/.hutch/worktrees/b/main.go","other":"/home/me/app-2/x"}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}
