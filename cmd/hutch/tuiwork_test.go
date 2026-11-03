package main

import (
	"encoding/json"
	"fmt"
	"os"
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
		adapters: agents.Builtin(), agent: "claude", splitPct: 50, taskLines: 5, taskHistory: 3, taskExpiry: time.Hour,
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

// Searching and filtering narrow the list without touching the machine (it
// has none here), keep the selection by identity, say what narrows it, and
// clear back to everything.
func TestSearchAndFilter(t *testing.T) {
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
	press(t, m, "enter")
	if !strings.Contains(m.filterLine(), `"login"`) {
		t.Fatalf("filter line: %q", m.filterLine())
	}

	// Every project: a box in another project is found, and enter opens its tab.
	press(t, m, "esc", "/", "metrics")
	if len(m.rows) != 0 {
		t.Fatalf("metrics in this project only: %v", rowKeys(m))
	}
	press(t, m, "tab")
	if got := rowKeys(m); strings.Join(got, ",") != "wise-elk,wise-elk/2" {
		t.Fatalf("metrics in every project: %v", got)
	}
	press(t, m, "down", "enter")
	if m.project() != "github.com/b/api" || m.selected().key() != "wise-elk/2" || m.filtering() {
		t.Fatalf("jump: tab %s, selected %s, filtering %v", m.project(), m.selected().key(), m.filtering())
	}

	m.switchTab("github.com/a/app")
	m.read = map[string]bool{} // selecting it above read it
	m.filter = filterState{state: "unread"}
	m.build()
	if got := rowKeys(m); strings.Join(got, ",") != "quiet-fox,quiet-fox/1" {
		t.Fatalf("unread: %v", got)
	}
	m.filter = filterState{state: "asleep"}
	m.build()
	if got := rowKeys(m); strings.Join(got, ",") != "bold-owl,bold-owl/1" {
		t.Fatalf("asleep: %v", got)
	}
	m.filter = filterState{state: "unreachable"}
	m.build()
	if got := rowKeys(m); strings.Join(got, ",") != "bold-owl,bold-owl/1" {
		t.Fatalf("on an offline machine: %v", got)
	}
	press(t, m, "F")
	if got := rowKeys(m); strings.Join(got, ",") != strings.Join(all, ",") || m.filtering() {
		t.Fatalf("cleared: %v, want %v", got, all)
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
	m.tasks = []*task{{ID: "1", Title: "move quiet-fox", State: taskFailed, Args: []string{"move", "quiet-fox"}, Lines: []string{"a", "b"}, Ended: time.Now()}}
	m.build()
	for _, size := range [][2]int{{80, 24}, {200, 60}} {
		m.w, m.h = size[0], size[1]
		m.resize()
		for _, open := range []func(){
			func() { m.setView(viewNone) },
			func() { m.setView(viewHelp) },
			func() { m.openTasks(nil) },
			func() { m.openTasks(m.tasks[0]) },
			func() { m.openMachines() },
			func() { m.infov = &infoView{box: m.boxes[1]}; m.setView(viewInfo) },
			func() { m.portv = &portsView{box: m.boxes[1]}; m.setView(viewPorts) },
			func() { m.setv = &settingsView{}; m.setView(viewSettings) },
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
		if !strings.Contains(help, a.name) {
			t.Errorf("help leaves out %q", a.name)
		}
	}
}

// Two panes side by side when there's room, stacked when it's tall, and one
// (the focused) when there's room for neither.
func TestSplitLayout(t *testing.T) {
	m := testTUI(t)
	m.pin = "quiet-fox/1"
	for _, c := range []struct {
		w, h             int
		split, collapsed bool
		beside           bool
	}{
		{200, 50, true, false, true},
		{100, 50, true, false, false},
		{90, 12, false, true, false},
	} {
		m.w, m.h = c.w, c.h
		main, second, split, collapsed := m.panes2()
		if split != c.split || collapsed != c.collapsed {
			t.Fatalf("%dx%d: split %v collapsed %v", c.w, c.h, split, collapsed)
		}
		if split && (second.x > main.x) != c.beside {
			t.Fatalf("%dx%d: main %+v second %+v", c.w, c.h, main, second)
		}
		if split && (main.w < minPaneW && c.beside || second.w < minPaneW && c.beside) {
			t.Fatalf("%dx%d: a pane narrower than %d", c.w, c.h, minPaneW)
		}
	}
	// What's visible: the pinned pane when split; when collapsed, only the
	// focused one. A hidden terminal neither marks read nor keeps its box awake.
	m.w, m.h = 200, 50
	m.shown.Store("wise-elk/2")
	if !m.visible("quiet-fox/1") || !m.visible("wise-elk/2") {
		t.Fatal("split: both panes should be visible")
	}
	m.w, m.h, m.focus2 = 90, 12, false
	if m.visible("quiet-fox/1") || !m.visible("wise-elk/2") {
		t.Fatal("collapsed, workspace focused: only the workspace is visible")
	}
	m.focus2 = true
	if !m.visible("quiet-fox/1") || m.visible("wise-elk/2") {
		t.Fatal("collapsed, second focused: only the second is visible")
	}
}

// Task output keeps its newest lines and counts the rest; history keeps the
// newest finished tasks within their expiry; kept tasks come back after a
// restart, and one that was running comes back as unknown.
func TestTaskHistory(t *testing.T) {
	m := testTUI(t)
	tk := &task{ID: "1", Title: "long", State: taskRunning, Started: time.Now()}
	m.tasks = append(m.tasks, tk)
	for i := range 8 {
		m.taskLine(taskLineMsg{"1", fmt.Sprintf("line %d", i)})
	}
	if len(tk.Lines) != 5 || tk.Dropped != 3 || tk.Lines[0] != "line 3" {
		t.Fatalf("kept %v, dropped %d", tk.Lines, tk.Dropped)
	}
	for i := range 4 {
		m.tasks = append(m.tasks, &task{ID: fmt.Sprint(10 + i), Title: "old", State: taskOK, Ended: time.Now().Add(-time.Duration(4-i) * time.Minute)})
	}
	m.tasks = append(m.tasks, &task{ID: "expired", State: taskFailed, Ended: time.Now().Add(-2 * time.Hour)})
	m.trimTasks()
	var ids []string
	for _, t := range m.tasks {
		ids = append(ids, t.ID)
	}
	if strings.Join(ids, ",") != "1,11,12,13" {
		t.Fatalf("after trimming: %v", ids)
	}
	m2 := testTUI(t)
	m2.taskHistory, m2.taskExpiry = 10, time.Hour
	m2.tasks = nil
	// Write one running task as an old version would have, then load.
	m.tasks = []*task{{ID: "x", Title: "was running", State: taskRunning, Started: time.Now(), Ended: time.Now()}}
	if err := writeTasksForTest(m.tasks); err != nil {
		t.Fatal(err)
	}
	m2.loadTasks()
	if len(m2.tasks) != 1 || m2.tasks[0].State != taskUnknown || m2.tasks[0].listed() {
		t.Fatalf("loaded: %+v", m2.tasks)
	}
}

func writeTasksForTest(ts []*task) error {
	data, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(tasksFile()), 0o700)
	return os.WriteFile(tasksFile(), data, 0o600)
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
