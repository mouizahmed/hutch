package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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
	v := m.gitv
	m.Update(gitStatusMsg{box: "quiet-fox", asked: v.asked, status: boxgit.Status{Branch: "main", Files: []boxgit.File{a, b}}})

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
	// A status read before the latest one is dropped when it comes last.
	older := v.asked
	m.gitRefresh()
	m.Update(gitStatusMsg{box: "quiet-fox", asked: v.asked, status: boxgit.Status{Branch: "main", Files: []boxgit.File{staged, a, b}}})
	m.Update(gitStatusMsg{box: "quiet-fox", asked: older, status: boxgit.Status{Branch: "main"}})
	if len(v.status.Files) != 3 {
		t.Fatalf("an older status replaced a newer one: %+v", v.status.Files)
	}
	if f, _ := v.selected(); f != a {
		t.Fatalf("the cursor left the shown file for %+v", f)
	}
	press(t, m, "u")
	if v.busy != "" {
		t.Fatalf("u on the unstaged diff did %q", v.busy)
	}

	// The task diff shows no file, so file keys do nothing there.
	press(t, m, "esc", "tab")
	m.Update(gitDiffMsg{box: "quiet-fox", want: v.want, branch: true, diff: boxgit.Diff{Text: "task diff"}})
	if !v.branch || v.diff == nil {
		t.Fatal("the task diff didn't open")
	}
	press(t, m, "a", "u")
	if v.busy != "" {
		t.Fatalf("a file key on the task diff did %q", v.busy)
	}
}

// The palette survives a search that matches nothing.
func TestPaletteNoMatch(t *testing.T) {
	m := testTUI(t)
	press(t, m, "space", "zzzz nothing", "down", "enter")
	if m.menu == nil {
		t.Fatal("the palette closed")
	}
}

// A click selects the row under it, with the search's line above the list too.
func TestClickWhileSearching(t *testing.T) {
	m := testTUI(t)
	press(t, m, "/", "fox", "enter")
	m.View()
	// Tabs, the machine line, the search's line, then the rows.
	m.Update(tea.MouseClickMsg{X: 3, Y: 3, Button: tea.MouseLeft})
	if got := m.selected().key(); got != m.rows[0].key() {
		t.Fatalf("clicked the first row, selected %s", got)
	}
}

// A prompt whose answer opens another (another local port, when the one asked
// for is taken) leaves that one open; empty there takes any free port.
func TestPromptOpensAnother(t *testing.T) {
	m := testTUI(t)
	m.ctx = t.Context()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	m.input("Forward", "", "", func(string) tea.Cmd { return m.startForward("quiet-fox", 8000, port) })
	press(t, m, "enter")
	if m.prompt == nil || !strings.Contains(m.prompt.title, "is taken") {
		t.Fatalf("the taken-port prompt didn't stay open: %+v", m.prompt)
	}
	press(t, m, "enter")
	f := m.forwards[forwardKey("quiet-fox", 8000)]
	if f == nil || f.local == 0 || f.local == port {
		t.Fatalf("empty didn't take a free port: %+v", f)
	}
	m.stopForward(f)
}

// A new box that fails gives its prompt back to the next form only, so it can't
// turn up in an unrelated one later.
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
	// Sent again and failing again, it's kept again.
	m.form.dir = t.TempDir()
	m.Update(m.submitForm()())
	m.openForm("")
	if m.form.prompt.Value() == "" {
		t.Fatal("the second failure didn't keep the prompt")
	}
	press(t, m, "esc")
	if m.openForm(""); m.form.prompt.Value() != "" {
		t.Fatalf("a prompt dismissed with esc came back: %q", m.form.prompt.Value())
	}
	press(t, m, "esc")

	// Two started before either fails each come back, one per form.
	var ends []tea.Cmd
	for _, p := range []string{"first", "second"} {
		m.openForm("")
		m.form.dir = t.TempDir()
		m.form.prompt.SetValue(p)
		ends = append(ends, m.submitForm())
	}
	for _, end := range ends {
		m.Update(end())
	}
	for _, want := range []string{"second", "first", ""} {
		if m.openForm(""); m.form.prompt.Value() != want {
			t.Fatalf("the form came back with %q, want %q", m.form.prompt.Value(), want)
		}
		press(t, m, "esc")
	}
}

// The template's row says where the template stands and the one key that takes
// it on, whether or not a template box exists.
func TestTemplatePane(t *testing.T) {
	m := testTUI(t)
	const app = "github.com/a/app"
	m.here = app
	m.tmplInfo = map[string]templateInfo{}
	m.projectInfo = map[string]api.Project{}
	m.build()
	m.selectKey("template " + app)
	r := m.selected()
	if r.kind != rowTemplate {
		t.Fatalf("selected %s", r.key())
	}
	pane := func() string {
		s, _ := m.paneView(100, 30)
		return ansi.Strip(s)
	}
	expect := func(stage string, want ...string) {
		t.Helper()
		got := pane()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: no %q in\n%s", stage, w, got)
			}
		}
	}
	expect("not set up", "no .hutch/ yet", "next  i sets it up")

	os.MkdirAll(filepath.Join(m.cwd, ".hutch"), 0o755)
	os.WriteFile(filepath.Join(m.cwd, ".hutch", "Dockerfile"), []byte("FROM x\n"), 0o644)
	expect("set up, unread", "✓ set up", "recipe        reading")

	m.tmplInfo[app] = templateInfo{since: -1, cpu: 2, memMiB: 4096, disk: "20G"}
	expect("unconfirmed", "not built yet", "not confirmed", "next  b or S first asks you to confirm the size")
	// b goes to the CLI, which asks for the size, rather than building in the background.
	m.templateKey(app, "b")
	if strings.Contains(m.status, "checking") || len(m.tasks) > 0 {
		t.Fatalf("b built without asking for the size: %q", m.status)
	}

	m.tmplInfo[app] = templateInfo{changed: 1, since: -1, cpu: 2, memMiB: 4096, disk: "20G", confirmed: true}
	m.projectInfo[app] = api.Project{ID: app, Peaks: &api.Peaks{MemMiB: 2150, CPUs: 1.3, OfVCPUs: 2, OfMiB: 4096}}
	expect("changed", "1 uncommitted change(s)", "2 vCPU · 4.0 GB · 20G disk", "at most 1.3 of 2 vCPUs", "next  b builds and checks")
	m.templateKey(app, "b")
	if !strings.Contains(m.status, "checking") {
		t.Fatalf("a confirmed size didn't build in the background: %q", m.status)
	}

	m.templates[app] = []api.Template{{Version: 3, Created: time.Now()}}
	m.tmplInfo[app] = templateInfo{cpu: 2, memMiB: 4096, disk: "20G", confirmed: true}
	expect("up to date", "v3 ·", "as v3", "next  up to date")

	// With a template box, its row is the template's, and Enter there opens the
	// agent waiting in it.
	m.boxes = append(m.boxes, api.Box{ID: "app-template", Project: app, Kind: api.BoxTemplate, State: "running", Worker: "w-1",
		Threads: []api.Thread{{Box: "app-template", N: 1, Agent: "claude", State: api.ThreadWaiting}, {Box: "app-template", N: 2, State: api.ThreadWorking}}})
	m.build()
	m.selectKey("app-template")
	expect("template box", "app-template awake", "1 claude waiting", "Enter opens terminal 1")
	press(t, m, "enter")
	if got := m.selected().key(); got != "app-template/1" {
		t.Fatalf("Enter on the template box selected %s", got)
	}
}

// Machines are labelled by role; an old hutchd that doesn't say which leads
// isn't called a worker.
func TestMachineRoles(t *testing.T) {
	m := testTUI(t)
	one := []api.Worker{{ID: "a", Name: "a"}}
	if got := m.role(one[0], one); got != "lead" {
		t.Fatalf("a lone machine: %q", got)
	}
	two := []api.Worker{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}
	if got := m.role(two[0], two); !strings.Contains(got, "unknown") {
		t.Fatalf("two machines, no lead said: %q", got)
	}
	two[1].Lead = true
	if m.role(two[0], two) != "worker" || m.role(two[1], two) != "lead" {
		t.Fatal("roles from the lead field")
	}
}

// What changed in the recipe since a version: hutch.toml, read here and never
// built from, doesn't count.
func TestRecipeChanges(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(name, text string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	write(".hutch/Dockerfile", "FROM a\n")
	write(".hutch/hutch.toml", "version = 1\n")
	run("add", "-A")
	run("commit", "-qm", "v1")
	v1 := run("rev-parse", "HEAD")
	if c, s := recipeChanges(dir, v1); c != 0 || s != 0 {
		t.Fatalf("as built: %d changed, %d since", c, s)
	}
	write(".hutch/hutch.toml", "version = 1\nsize.cpu = 4\n")
	write("main.go", "package main\n")
	if c, _ := recipeChanges(dir, v1); c != 0 {
		t.Fatalf("hutch.toml or code counted as recipe changes: %d", c)
	}
	write(".hutch/Dockerfile", "FROM b\n")
	run("commit", "-qam", "newer base")
	write(".hutch/on-create.sh", "true\n")
	if c, s := recipeChanges(dir, v1); c != 1 || s != 1 {
		t.Fatalf("one commit and one new file: %d changed, %d since", c, s)
	}
	if _, s := recipeChanges(dir, "0123456789abcdef0123456789abcdef01234567"); s != -1 {
		t.Fatalf("a version commit that isn't here: %d since", s)
	}
}
