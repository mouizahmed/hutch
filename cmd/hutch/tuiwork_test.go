package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/boxgit"
)

// testTUI is a TUI with two projects' boxes and no machine connection: what
// it does must not need one.
func testTUI(t *testing.T) *tui {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	m := &tui{w: 120, h: 40, focused: true, listWidth: 44, kept: 4, reached: true, synced: true,
		projects: map[string]string{"github.com/a/app": "app", "github.com/b/api": "api"}, templates: map[string][]api.Template{},
		panes: map[string]*pane{}, read: map[string]bool{}, forwards: map[string]*forward{}, models: map[string]string{},
		adapters: agents.Builtin(), agent: "claude", taskLines: 5,
		active: "github.com/a/app", cwd: t.TempDir(), offline: true}
	m.shown.Store("")
	m.workers = []api.Worker{{ID: "w-1", Name: "lead", Online: true, Lead: true}, {ID: "w-2", Name: "gpu", Online: false}}
	m.boxes = []api.Box{
		{ID: "quiet-fox", Project: "github.com/a/app", State: "running", Worker: "w-1", Base: "main", Created: time.Now(),
			Threads: []api.Thread{{Box: "quiet-fox", N: 1, Agent: "claude", Title: "fix login", State: api.ThreadWaiting, Unread: true}}},
		{ID: "bold-owl", Project: "github.com/a/app", State: "asleep", Worker: "w-2", Created: time.Now().Add(-time.Hour),
			Threads: []api.Thread{{Box: "bold-owl", N: 1, Agent: "codex", Title: "write docs", State: api.ThreadExited}}},
		{ID: "wise-elk", Project: "github.com/b/api", State: "running", Worker: "w-1", Created: time.Now(),
			Threads: []api.Thread{{Box: "wise-elk", N: 2, Agent: "codex", Title: "add metrics", State: api.ThreadWorking}}},
	}
	m.build()
	return m
}

func press(t *testing.T, m *tui, ks ...string) {
	t.Helper()
	for _, k := range ks {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "space":
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		default:
			for _, r := range k {
				msg = tea.KeyPressMsg{Code: r, Text: string(r)}
				m.Update(msg)
			}
			continue
		}
		m.Update(msg)
	}
}

func rowKeys(m *tui) []string {
	var out []string
	for _, r := range m.rows {
		out = append(out, r.key())
	}
	return out
}

// No two actions that apply at once share a key, wherever the cursor is.
func TestActionKeysUnique(t *testing.T) {
	m := testTUI(t)
	for i := range m.rows {
		m.cursor = i
		seen := map[string]string{}
		for _, a := range m.actions() {
			if a.name == "" || a.do == nil {
				t.Fatalf("an action without a name or a function: %+v", a)
			}
			if a.key == "" || a.why != "" {
				continue
			}
			if other, ok := seen[a.key]; ok {
				t.Errorf("row %s: %q is both %q and %q", m.rows[i].key(), a.key, other, a.name)
			}
			seen[a.key] = a.name
		}
	}
}

// Search narrows the open tab's list without touching the machine (it has none
// here), keeps the selection by identity, and esc clears it back to everything.
func TestSearch(t *testing.T) {
	m := testTUI(t)
	all := rowKeys(m)
	m.selectKey("quiet-fox/1")

	press(t, m, "/", "login")
	if got := rowKeys(m); strings.Join(got, ",") != "quiet-fox,quiet-fox/1" {
		t.Fatalf("search login: %v", got)
	}
	if m.selected().key() != "quiet-fox/1" {
		t.Fatalf("the selection moved to %s", m.selected().key())
	}
	press(t, m, "enter", "esc")
	if got := rowKeys(m); strings.Join(got, ",") != strings.Join(all, ",") || m.search.query != "" {
		t.Fatalf("cleared: %v, want %v", got, all)
	}
	// Machines match by name; another project's boxes don't show.
	press(t, m, "/", "gpu")
	if got := rowKeys(m); strings.Join(got, ",") != "bold-owl,bold-owl/1" {
		t.Fatalf("search gpu: %v", got)
	}
	press(t, m, "esc", "/", "metrics")
	if len(m.rows) != 0 {
		t.Fatalf("another project's box showed: %v", rowKeys(m))
	}
}

// The palette finds actions by any words of their names, and explains one that
// doesn't apply rather than running it.
func TestPalette(t *testing.T) {
	m := testTUI(t)
	m.selectKey("bold-owl")
	press(t, m, "space")
	if m.menu == nil || !m.menu.search {
		t.Fatal("space didn't open the palette")
	}
	press(t, m, "pull req")
	shown := m.menu.shown()
	if len(shown) != 1 || shown[0].label != "Open a pull request" {
		t.Fatalf("pull req: %+v", shown)
	}
	if !strings.Contains(shown[0].why, "no repository") {
		t.Fatalf("bold-owl has no base: why %q", shown[0].why)
	}
	press(t, m, "enter")
	if m.menu == nil || !strings.Contains(m.status, "no repository") {
		t.Fatalf("an action that doesn't apply ran or closed the palette: %q", m.status)
	}
	press(t, m, "esc")
	if m.menu != nil {
		t.Fatal("esc didn't close the palette")
	}
}

// Every screen fits 80x24 and a wide terminal: no line wider than the screen,
// and the footer always says the mode.
func TestScreensFit(t *testing.T) {
	m := testTUI(t)
	m.tasks = []*task{{ID: "1", Title: "move quiet-fox", State: taskFailed, Lines: []string{"a", "b"}, Err: "b"}}
	m.build()
	for _, size := range [][2]int{{80, 24}, {200, 60}} {
		m.w, m.h = size[0], size[1]
		m.resize()
		for _, open := range []func(){
			func() { m.setView(viewNone) },
			func() { m.setView(viewHelp) },
			func() { m.openMachines() },
			func() { m.portv = &portsView{box: m.boxes[1]}; m.setView(viewPorts) },
			func() { m.openPalette() },
		} {
			m.menu = nil
			open()
			v := m.View()
			lines := strings.Split(v.Content, "\n")
			if len(lines) > m.h {
				t.Fatalf("%dx%d view %d: %d lines", m.w, m.h, m.view, len(lines))
			}
			for _, l := range lines {
				if w := lipgloss.Width(l); w > m.w {
					t.Fatalf("%dx%d view %d: a line %d wide: %q", m.w, m.h, m.view, w, l)
				}
			}
			if !strings.Contains(lines[len(lines)-1], " ") {
				t.Fatalf("no footer: %q", lines[len(lines)-1])
			}
		}
	}
	m.menu = nil
	m.setView(viewHelp)
	help := m.helpView(200, 1000)
	for _, a := range m.actions() {
		if a.key != "" && !strings.Contains(help, a.name) {
			t.Errorf("help leaves out %q", a.name)
		}
	}
}

// A terminal under a view isn't on screen: it doesn't count as seen, and its
// box may sleep.
func TestViewHidesTheTerminal(t *testing.T) {
	m := testTUI(t)
	m.shown.Store("quiet-fox/1")
	if !m.visible("quiet-fox/1") {
		t.Fatal("the shown terminal isn't visible")
	}
	m.setView(viewHelp)
	if m.visible("quiet-fox/1") || m.hiddenMark("quiet-fox/1") == "" {
		t.Fatal("a terminal under a view counts as seen")
	}
}

// A task keeps its newest output lines and counts the rest; one that ends well
// leaves the list, and one that failed stays until d dismisses it.
func TestTaskRows(t *testing.T) {
	m := testTUI(t)
	tk := &task{ID: "1", Title: "long", State: taskRunning, Started: time.Now()}
	ok := &task{ID: "2", Title: "quick", State: taskRunning, Started: time.Now()}
	m.tasks = append(m.tasks, tk, ok)
	for i := range 8 {
		m.taskLine(taskLineMsg{"1", fmt.Sprintf("line %d", i)})
	}
	if len(tk.Lines) != 5 || tk.Dropped != 3 || tk.Lines[0] != "line 3" {
		t.Fatalf("kept %v, dropped %d", tk.Lines, tk.Dropped)
	}
	m.taskDone(taskDoneMsg{"1", errors.New("exit status 1")})
	m.taskDone(taskDoneMsg{"2", nil})
	if len(m.tasks) != 1 || m.tasks[0] != tk || tk.State != taskFailed || tk.Err != "line 7" {
		t.Fatalf("after ending: %+v", m.tasks)
	}
	m.cursor = 0
	if r := m.selected(); r.kind != rowTask {
		t.Fatalf("the failed task isn't listed first: %+v", r)
	}
	press(t, m, "d")
	if len(m.tasks) != 0 {
		t.Fatal("d didn't dismiss the failed task")
	}
}

// The Git view's keys act on the diff shown, as staging regroups the list
// under it, and a diff closed before it arrived stays closed.
func TestGitViewKeepsItsTarget(t *testing.T) {
	m := testTUI(t)
	m.openGit(m.boxes[0])
	a, b := boxgit.File{Path: "a.go", Kind: boxgit.Unstaged}, boxgit.File{Path: "b.go", Kind: boxgit.Unstaged}
	m.Update(gitStatusMsg{box: "quiet-fox", status: boxgit.Status{Branch: "main", Files: []boxgit.File{a, b}}})
	v := m.gitv

	press(t, m, "enter", "esc")
	m.Update(gitDiffMsg{box: "quiet-fox", want: 1, file: a, diff: boxgit.Diff{Text: "late"}})
	if v.diff != nil || m.view != viewGit {
		t.Fatal("a diff closed while it was read came back")
	}

	press(t, m, "enter")
	m.Update(gitDiffMsg{box: "quiet-fox", want: v.want, file: a, diff: boxgit.Diff{Text: "diff"}})
	if v.diff == nil {
		t.Fatal("the diff didn't open")
	}
	staged := boxgit.File{Path: "a.go", Kind: boxgit.Staged}
	m.Update(gitStatusMsg{box: "quiet-fox", status: boxgit.Status{Branch: "main", Files: []boxgit.File{staged, a, b}}})
	if f, _ := v.selected(); f != a {
		t.Fatalf("the cursor left the shown file for %+v", f)
	}
	press(t, m, "u")
	if v.busy != "" {
		t.Fatalf("u on the unstaged diff did %q", v.busy)
	}
}

// A new box that fails gives its prompt back to the next form; one sent again
// doesn't linger.
func TestFailedTaskKeepsPrompt(t *testing.T) {
	m := testTUI(t)
	m.exe = filepath.Join(t.TempDir(), "missing") // the command can't start
	m.openForm("")
	m.form.dir = t.TempDir()
	m.form.prompt.SetValue("fix the flaky login test\nand add a regression test")
	cmd := m.submitForm()
	if cmd == nil {
		t.Fatalf("a command that can't start reported nothing (form: %q)", m.form.err)
	}
	m.Update(cmd())
	if len(m.tasks) != 1 || m.tasks[0].State != taskFailed || !strings.Contains(m.status, "brings back") {
		t.Fatalf("tasks %+v, status %q", m.tasks, m.status)
	}
	m.openForm("")
	if got := m.form.prompt.Value(); got != "fix the flaky login test\nand add a regression test" {
		t.Fatalf("the form came back with %q", got)
	}
	press(t, m, "esc")
	m.openForm("")
	if m.form.prompt.Value() == "" {
		t.Fatal("esc lost the kept prompt")
	}
	// Sent again, it's no longer kept; failing again keeps it again.
	m.form.dir = t.TempDir()
	cmd = m.submitForm()
	if m.draft != "" {
		t.Fatal("a prompt sent again is still kept")
	}
	m.Update(cmd())
	if m.openForm(""); m.form.prompt.Value() == "" {
		t.Fatal("the second failure didn't keep the prompt")
	}
}
