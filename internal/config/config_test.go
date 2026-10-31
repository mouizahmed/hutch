package config

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, l Layer, text string) *Doc {
	t.Helper()
	d, err := Parse(l, l.String(), []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		layer      Layer
		text, want string
	}{
		{User, "idle_timout = \"5m\"", "unknown key idle_timout"},
		{Project, "machine = \"me@host\"", "can't be set in the project config"},
		{Project, "idle_timeout = \"10s\"", "below the minimum 1m"},
		{Project, "size = { cpu = \"4\" }", "size.cpu: want a whole number"},
		{User, "version = 2", "version 2 is not supported"},
	} {
		_, err := Parse(tc.layer, "f", []byte(tc.text))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.text, err, tc.want)
		}
	}
}

// allowlist isn't built: a box must refuse it, but a stored value must still load,
// or every setting (backups among them) would stop with it.
func TestAllowlistLoadsButRefuses(t *testing.T) {
	c := New(mustParse(t, User, `network.egress = "allowlist"`))
	if c.Unsupported() != ErrAllowlist {
		t.Error("allowlist from the user layer not refused")
	}
	if c.Duration("idle_timeout") == 0 {
		t.Error("other settings unreadable")
	}
	c = New(mustParse(t, User, "network.egress = \"allowlist\"\n[grants.\"github.com/me/app\"]\nnetwork.egress = \"allowlist\""),
		mustParse(t, Project, `network.egress = "allowlist"`))
	c.ProjectID = "github.com/me/app"
	if c.Unsupported() != ErrAllowlist {
		t.Error("allowlist requested by a project not refused")
	}
}

func TestPrecedence(t *testing.T) {
	c := New(mustParse(t, User, `default_agent = "codex"`), mustParse(t, Project, "idle_timeout = \"30m\"\n[size]\ncpu = 8"))
	if s, _ := c.Get("default_agent"); s.Value != "codex" || s.Layer != User {
		t.Errorf("default_agent = %v from %s", s.Value, s.Layer)
	}
	c.SetFlag("default_agent", "claude")
	if c.String("default_agent") != "claude" || c.Int("size.cpu") != 8 || c.Size("size.memory") != 8<<30 {
		t.Error("flag, project, or default not applied")
	}
}

// A cloned repo must not get more than the user allows or has granted.
func TestRequestsLimitedByGrants(t *testing.T) {
	proj := mustParse(t, Project, `autonomy = "full"
credentials = ["claude", "gh"]
network.allow = ["10.0.5.20:5432", "10.0.0.1:22"]`)
	usr := mustParse(t, User, `autonomy = "ask"
credentials = ["claude"]
[grants."github.com/me/app"]
network.allow = ["10.0.5.20:5432"]`)
	c := New(usr, proj)
	c.ProjectID = "github.com/me/app"

	checks := map[string][2]string{
		"autonomy":      {"ask", "full"},
		"credentials":   {"[claude]", "[gh]"},
		"network.allow": {"[10.0.5.20:5432]", "[10.0.0.1:22]"},
	}
	for key, want := range checks {
		s, _ := c.Get(key)
		if s.String() != want[0] || Display(s.Withheld) != want[1] {
			t.Errorf("%s = %s withheld %s, want %s withheld %s", key, s, Display(s.Withheld), want[0], want[1])
		}
	}

	// Another project gets nothing from this project's grants.
	c.ProjectID = "github.com/evil/app"
	if s, _ := c.Get("network.allow"); s.String() != "[]" {
		t.Errorf("grant leaked to another project: %s", s)
	}
	c.ProjectID = "github.com/me/app"

	text, err := c.Grant(usr.Text, c.Withheld())
	if err != nil {
		t.Fatal(err)
	}
	c = New(mustParse(t, User, string(text)), proj)
	c.ProjectID = "github.com/me/app"
	if w := c.Withheld(); len(w) != 0 {
		t.Errorf("still withheld after granting: %v\n%s", w, text)
	}
	if s, _ := c.Get("autonomy"); s.Value != "full" {
		t.Errorf("autonomy = %v after grant", s.Value)
	}
}

func TestEditKeepsComments(t *testing.T) {
	text := []byte(`# my settings
version = 1
default_agent = "codex" # I prefer codex

[grants."github.com/me/app"]
# reviewed 2026-10
credentials = [
  "claude",
]
`)
	out, err := Set(text, []string{"machine"}, "me@box")
	if err != nil {
		t.Fatal(err)
	}
	out, err = Set(out, []string{"grants", "github.com/me/app", "credentials"}, []any{"claude", "gh"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = Set(out, []string{"grants", "github.com/other/x", "autonomy"}, "full")
	if err != nil {
		t.Fatal(err)
	}
	want := `# my settings
version = 1
default_agent = "codex" # I prefer codex
machine = "me@box"

[grants."github.com/me/app"]
# reviewed 2026-10
credentials = ["claude", "gh"]

[grants."github.com/other/x"]
autonomy = "full"
`
	if string(out) != want {
		t.Fatalf("got:\n%s", out)
	}
	out, err = Remove(out, []string{"grants", "github.com/me/app"})
	if err != nil {
		t.Fatal(err)
	}
	mustParse(t, User, string(out))
	if strings.Contains(string(out), "me/app") || !strings.Contains(string(out), "other/x") {
		t.Fatalf("after remove:\n%s", out)
	}
}

// The setup prompt has agents write size as an inline table with a reason.
func TestEditInlineTable(t *testing.T) {
	text := []byte("version = 1\nsize = { cpu = 4, memory = \"8G\" }   # Next.js + Postgres\n")
	if c := Comment(text, []string{"size", "memory"}); c != "Next.js + Postgres" {
		t.Errorf("comment %q", c)
	}
	out, err := Set(text, []string{"size", "memory"}, "16G")
	if err != nil {
		t.Fatal(err)
	}
	if want := "version = 1\nsize = { cpu = 4, memory = \"16G\" }   # Next.js + Postgres\n"; string(out) != want {
		t.Fatalf("got %q", out)
	}
}

func TestNormalizeRemote(t *testing.T) {
	for _, url := range []string{"git@github.com:me/app.git", "https://github.com/me/app", "ssh://git@GitHub.com/me/app.git/"} {
		if got := NormalizeRemote(url); got != "github.com/me/app" {
			t.Errorf("%s -> %s", url, got)
		}
	}
}
