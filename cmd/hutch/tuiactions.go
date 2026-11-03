package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mouizahmed/hutch/internal/api"
)

// Actions are everything the TUI can do, each with its key, where it applies,
// and why not when it doesn't. One list drives the keys, the action palette
// (space), and the help view (?), so they can't disagree.

type viewKind int

const (
	viewNone viewKind = iota
	viewGit
	viewPorts
	viewMachines
	viewHelp
)

var viewNames = map[viewKind]string{viewGit: "Git", viewPorts: "Ports", viewMachines: "Machines", viewHelp: "Help"}

// setView shows a view in the workspace pane, or with viewNone, the terminal
// again. The list keeps its selection either way.
func (m *tui) setView(v viewKind) {
	m.view = v
	m.typing = false
	m.resize()
	m.markHidden()
}

type action struct {
	group, key, name string
	why              string // why it doesn't apply now; empty when it does
	do               func() tea.Cmd
}

// actions lists what applies to the selected row and view, and what doesn't,
// with why.
func (m *tui) actions() []action {
	r := m.selected()
	b := r.box
	var as []action
	add := func(group, key, name, why string, do func() tea.Cmd) {
		as = append(as, action{group: group, key: key, name: name, why: why, do: do})
	}
	noBox := ""
	switch {
	case b.ID == "":
		noBox = "select a box first"
	case b.Kind == api.BoxVersion:
		noBox = "a template version is read-only"
	}
	noRepo := noBox
	if noRepo == "" && b.Base == "" && b.Kind != api.BoxTemplate {
		noRepo = b.ID + " has no repository: only boxes created in a project have one"
	}
	noTerm := ""
	if r.kind != rowThread {
		noTerm = "select a terminal first"
	}
	project, dir := m.newProject()
	noCheckout := ""
	switch {
	case project == "":
		noCheckout = "open a project's tab first"
	case dir == "":
		noCheckout = "this project needs a checkout here: p → Add a checkout"
	}
	boxCheckout := noBox
	if boxCheckout == "" {
		if b.Project == "" {
			boxCheckout = b.ID + " isn't in a project"
		} else if m.checkout(b.Project) == "" {
			boxCheckout = m.projectName(b.Project) + " needs a checkout here: p → Add a checkout"
		}
	}
	tmpl := templateOf(r)
	noTmpl := ""
	if tmpl == "" {
		noTmpl = "select the template row first"
	}

	// Boxes and terminals.
	add("Box", "n", "New box", "", func() tea.Cmd { return m.openForm("") })
	add("Box", "N", "New terminal in this box", noBox, func() tea.Cmd { return m.openForm(b.ID) })
	add("Box", "enter", "Type into the terminal, or wake its box (ctrl+] returns)", noBox, func() tea.Cmd { return m.enter(r) })
	sleepName := "Sleep"
	if b.ID != "" && b.State != "running" {
		sleepName = "Wake"
	}
	add("Box", "s", sleepName, noBox, func() tea.Cmd { return m.sleepWake(b) })
	removeWhy := noBox
	if r.kind == rowTask {
		removeWhy = ""
	}
	add("Box", "d", "Remove the box or terminal (on a task: cancel or dismiss it)", removeWhy, func() tea.Cmd { return m.removeRow(r) })
	add("Box", "g", "Git: review changes, stage, commit, push", noRepo, func() tea.Cmd { return m.openGit(b) })
	add("Box", "o", "Ports: forward a service here", noBox, func() tea.Cmd { return m.openPorts(b) })
	add("Box", "", "Move to another machine", noBox, func() tea.Cmd { return m.moveBox(b) })
	add("Box", "", "Resize: CPUs, memory, disk", noBox, func() tea.Cmd { return m.resizeBox(b) })
	add("Box", "", "Open a pull request", noRepo, func() tea.Cmd {
		return m.confirmRun("Push "+b.ID+"'s branch and open a pull request from it?\n\nIt refuses uncommitted work.", m.cwd, "pull request from "+b.ID, "pr", b.ID)
	})
	add("Box", "", "Bring its work into this computer's checkout (hutch local)", boxCheckout, func() tea.Cmd {
		return m.interactive(m.checkout(b.Project), "local", b.ID)
	})
	add("Box", "", "Serial console (for a broken guest)", noBox, func() tea.Cmd { return m.interactive(m.cwd, "console", b.ID) })
	add("Terminal", "r", "Rename the terminal", noTerm, func() tea.Cmd { return m.renameTerminal(r) })
	add("Terminal", "z", "Zoom the pane", "", func() tea.Cmd { m.zoom = !m.zoom; m.resize(); return nil })
	add("Terminal", "\\", "Put the pane beside or below the list", "", func() tea.Cmd { m.below = !m.below; m.resize(); return nil })

	// Projects and templates.
	add("Project", "p", "Projects: open or add a checkout", "", func() tea.Cmd { m.openPicker(); return nil })
	add("Project", "x", "Close this tab", "", func() tea.Cmd { m.closeTab(m.project()); return nil })
	add("Project", "i", "Set up the project (hutch init)", noTmpl, func() tea.Cmd { return m.templateKey(tmpl, "i") })
	add("Project", "b", "Build the template", noTmpl, func() tea.Cmd { return m.templateKey(tmpl, "b") })
	add("Project", "S", "Snapshot the template as a new version", noTmpl, func() tea.Cmd { return m.templateKey(tmpl, "S") })
	add("Project", "", "Send this computer's work to a new box (hutch cloud)", noCheckout, func() tea.Cmd { return m.interactive(dir, "cloud") })
	add("Project", "", "Template versions", noCheckout, func() tea.Cmd { return m.interactive(dir, "template", "versions") })
	add("Project", "", "Check the template twice (doctor --template)", noCheckout, func() tea.Cmd {
		return m.interactive(dir, "doctor", "--template")
	})
	add("Project", "", "Remove the project's templates and secrets from the machine", noCheckout, func() tea.Cmd {
		return m.confirmRun("Remove "+m.projectName(project)+"'s templates and secrets from the machine?\n\nIts boxes stay; new boxes need a new template.", dir, "remove project "+m.projectName(project), "project", "rm")
	})

	// Views.
	add("View", "/", "Search this project's boxes and terminals", "", func() tea.Cmd { return m.startSearch() })
	add("View", "m", "Machines", "", func() tea.Cmd { m.openMachines(); return nil })
	add("View", "?", "Help: every key and action", "", func() tea.Cmd { m.setView(viewHelp); return nil })
	add("View", "[ ]", "Previous or next project tab", "", func() tea.Cmd { m.stepTab(1); return nil })

	// Settings and the machine.
	add("Settings", "", "Log in to an agent or GitHub", "", func() tea.Cmd { return m.loginMenu() })
	add("Settings", "", "Copy agent config and MCP servers here into every box (user sync)", "", func() tea.Cmd { return m.interactive(m.cwd, "user", "sync") })
	add("Settings", "", "Edit your config", "", func() tea.Cmd { return m.interactive(m.cwd, "config", "edit") })
	add("Settings", "", "Edit the machine config", "", func() tea.Cmd { return m.interactive(m.cwd, "config", "edit", "--machine") })
	add("Settings", "", "Edit the project's settings", noCheckout, func() tea.Cmd { return m.interactive(dir, "config", "edit", "--project") })
	add("Settings", "", "Check every config layer", "", func() tea.Cmd { return m.interactive(m.cwd, "config", "validate") })
	add("Settings", "", "Show grants", "", func() tea.Cmd { return m.interactive(m.cwd, "config", "grants") })
	add("Settings", "", "Revoke what's granted to this project", noCheckout, func() tea.Cmd {
		return m.confirmRun("Revoke everything granted to "+m.projectName(project)+"?\n\nIts boxes keep what they have until they're recreated.", dir, "revoke grants for "+m.projectName(project), "config", "grants", "--revoke")
	})
	add("Machine", "", "Doctor: check everything, with fixes", "", func() tea.Cmd { return m.interactive(m.cwd, "doctor") })
	add("Machine", "", "Box sizes and how many fit", "", func() tea.Cmd { return m.interactive(m.cwd, "sizes") })
	add("Machine", "", "Add a machine", "", func() tea.Cmd { return m.addMachine() })
	add("Machine", "", "Upgrade every machine", "", func() tea.Cmd { return m.interactive(m.cwd, "machine", "upgrade") })
	add("Machine", "", "Backup bucket", "", func() tea.Cmd { return m.interactive(m.cwd, "machine", "bucket") })
	add("Machine", "", "Delete unused snapshots and chunks (gc)", "", func() tea.Cmd {
		return m.confirmRun("Delete old box snapshots and bucket chunks nothing uses?", m.cwd, "machine gc", "machine", "gc")
	})
	add("Hutch", "q", "Quit", "", func() tea.Cmd { return m.quit() })
	return as
}

// key runs the action bound to a key, or says why it doesn't apply.
func (m *tui) actionKey(k string) (tea.Cmd, bool) {
	var why string
	for _, a := range m.actions() {
		if a.key != k {
			continue
		}
		if a.why == "" {
			return a.do(), true
		}
		why = a.why
	}
	if why != "" {
		m.setStatus(why)
		return nil, true
	}
	return nil, false
}

// A menu is a list to choose from: the action palette (filtered as you type), or
// a choice an action asks for.
type menu struct {
	title  string
	items  []menuItem
	cursor int
	filter textinput.Model
	search bool
}

type menuItem struct {
	label, detail, why string
	do                 func() tea.Cmd
}

func (m *tui) openPalette() tea.Cmd {
	var items []menuItem
	for _, a := range m.actions() {
		detail := a.group
		if a.key != "" {
			detail += " · " + a.key
		}
		items = append(items, menuItem{label: a.name, detail: detail, why: a.why, do: a.do})
	}
	f := textinput.New()
	f.Prompt = "› "
	f.Placeholder = "type to find an action"
	m.menu = &menu{title: "Actions", items: items, filter: f, search: true}
	return m.menu.filter.Focus()
}

func (m *tui) choose(title string, items []menuItem) {
	m.menu = &menu{title: title, items: items}
}

// shown is the menu's items that match its filter: every word, anywhere.
func (mn *menu) shown() []menuItem {
	q := strings.Fields(strings.ToLower(mn.filter.Value()))
	var out []menuItem
	for _, it := range mn.items {
		hay := strings.ToLower(it.label + " " + it.detail)
		if !slices.ContainsFunc(q, func(w string) bool { return !strings.Contains(hay, w) }) {
			out = append(out, it)
		}
	}
	return out
}

func (m *tui) menuKey(msg tea.KeyPressMsg) tea.Cmd {
	mn := m.menu
	items := mn.shown()
	switch msg.String() {
	case "esc":
		m.menu = nil
		return nil
	case "down", "ctrl+n", "tab":
		mn.cursor = min(mn.cursor+1, len(items)-1)
		return nil
	case "up", "ctrl+p", "shift+tab":
		mn.cursor = max(mn.cursor-1, 0)
		return nil
	case "enter":
		if mn.cursor >= len(items) {
			return nil
		}
		it := items[mn.cursor]
		if it.why != "" {
			m.setStatus(it.why)
			return nil
		}
		m.menu = nil
		return it.do()
	}
	if !mn.search {
		switch msg.String() {
		case "j":
			mn.cursor = min(mn.cursor+1, len(items)-1)
		case "k":
			mn.cursor = max(mn.cursor-1, 0)
		}
		return nil
	}
	var cmd tea.Cmd
	mn.filter, cmd = mn.filter.Update(msg)
	mn.cursor = 0
	return cmd
}

func (m *tui) menuView() string {
	mn := m.menu
	w := min(76, m.w-4)
	items := mn.shown()
	mn.cursor = max(min(mn.cursor, len(items)-1), 0)
	lines := []string{styleBold.Render(mn.title)}
	if mn.search {
		lines = append(lines, mn.filter.View())
	}
	lines = append(lines, "")
	rows := max(min(len(items), m.h-12), 1)
	top := max(mn.cursor-rows+1, 0)
	for i := top; i < len(items) && i < top+rows; i++ {
		it := items[i]
		label := it.label
		if it.why != "" {
			label = styleDim.Render(label)
		}
		line := spread(" "+truncate(label, w-24), styleDim.Render(truncate(it.detail, 20)), w-6)
		if i == mn.cursor {
			line = highlight(line, w-6, colorCursor)
		}
		lines = append(lines, line)
	}
	if len(items) == 0 {
		lines = append(lines, styleDim.Render("  nothing matches"))
	} else if why := items[mn.cursor].why; why != "" {
		lines = append(lines, "", styleError.Render(truncate("Not now: "+why, w-6)))
	}
	lines = append(lines, "", styleDim.Render("enter runs · ↑↓ choose · esc"))
	return stylePopup.Width(w).Render(strings.Join(lines, "\n"))
}

// A prompt asks for one or more values, or with multi, a message of lines.
type prompt struct {
	title, hint string
	fields      []textinput.Model
	labels      []string
	field       int
	area        *textarea.Model
	err         string
	submit      func(values []string) (tea.Cmd, string)
}

// input asks for one value.
func (m *tui) input(title, placeholder, value string, submit func(string) tea.Cmd) {
	m.form2(title, "enter · esc", []string{""}, []string{placeholder}, []string{value}, func(v []string) (tea.Cmd, string) {
		return submit(strings.TrimSpace(v[0])), ""
	})
}

// form2 asks for several labelled values; submit returns why they won't do,
// to fix and try again.
func (m *tui) form2(title, hint string, labels, placeholders, values []string, submit func([]string) (tea.Cmd, string)) {
	p := &prompt{title: title, hint: hint, labels: labels, submit: submit}
	for i := range labels {
		t := textinput.New()
		t.Prompt = ""
		t.Placeholder = placeholders[i]
		t.SetValue(values[i])
		t.SetWidth(min(60, m.w-20))
		p.fields = append(p.fields, t)
	}
	p.fields[0].Focus()
	m.prompt = p
}

// writeMessage asks for a message of several lines.
func (m *tui) writeMessage(title, placeholder string, submit func(string) (tea.Cmd, string)) {
	a := textarea.New()
	a.ShowLineNumbers = false
	a.Prompt = ""
	a.Placeholder = placeholder
	a.SetWidth(min(70, m.w-12))
	a.SetHeight(min(8, max(m.h-14, 3)))
	a.Focus()
	m.prompt = &prompt{title: title, hint: "ctrl+s saves · enter newline · esc", area: &a,
		submit: func([]string) (tea.Cmd, string) { return submit(a.Value()) }}
}

func (m *tui) promptKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.prompt
	k := msg.String()
	if k == "esc" {
		m.prompt = nil
		return nil
	}
	if p.area != nil {
		if k == "ctrl+s" {
			cmd, why := p.submit(nil)
			if why != "" {
				p.err = why
				return nil
			}
			m.prompt = nil
			return cmd
		}
		a, cmd := p.area.Update(msg)
		*p.area = a
		return cmd
	}
	switch k {
	case "enter":
		var vals []string
		for _, f := range p.fields {
			vals = append(vals, f.Value())
		}
		cmd, why := p.submit(vals)
		if why != "" {
			p.err = why
			return nil
		}
		m.prompt = nil
		return cmd
	case "tab", "down", "shift+tab", "up":
		d := 1
		if k == "shift+tab" || k == "up" {
			d = -1
		}
		p.fields[p.field].Blur()
		p.field = (p.field + d + len(p.fields)) % len(p.fields)
		return p.fields[p.field].Focus()
	}
	var cmd tea.Cmd
	p.fields[p.field], cmd = p.fields[p.field].Update(msg)
	return cmd
}

func (m *tui) promptPaste(msg tea.PasteMsg) tea.Cmd {
	p := m.prompt
	var cmd tea.Cmd
	if p.area != nil {
		a, c := p.area.Update(msg)
		*p.area, cmd = a, c
		return cmd
	}
	p.fields[p.field], cmd = p.fields[p.field].Update(msg)
	return cmd
}

func (m *tui) promptView() string {
	p := m.prompt
	w := min(80, m.w-4)
	lines := []string{styleBold.Render(p.title), ""}
	if p.area != nil {
		lines = append(lines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("39")).Render(p.area.View()))
	}
	for i, f := range p.fields {
		label := ""
		if p.labels[i] != "" {
			label = fmt.Sprintf("%-8s ", p.labels[i])
			if i == p.field && len(p.fields) > 1 {
				label = styleLabel.Render(label)
			} else {
				label = styleDim.Render(label)
			}
		}
		lines = append(lines, label+"› "+f.View())
	}
	if p.err != "" {
		lines = append(lines, "", styleError.Render(truncate(p.err, w-6)))
	}
	lines = append(lines, "", styleDim.Render(p.hint))
	return stylePopup.Width(w).Render(strings.Join(lines, "\n"))
}

// ask confirms before do, naming its target in text.
func (m *tui) ask(text, hint string, do func() tea.Cmd) {
	m.confirm, m.confirmText, m.confirmHint, m.confirmDo = "ask", text, hint, do
}

// confirmRun asks, then runs a hutch command as a task.
func (m *tui) confirmRun(text, dir, title string, args ...string) tea.Cmd {
	m.ask(text, "y runs it · any other key doesn't", func() tea.Cmd { return m.run(dir, title, "", args...) })
	return nil
}

// interactiveDoneMsg is an interactive command's end.
type interactiveDoneMsg struct {
	what string
	err  error
}

// interactive hands this terminal to a hutch command that asks things (a
// login, an editor, a review), and comes back when it ends. Its output isn't
// kept: it may hold what you typed.
func (m *tui) interactive(dir string, args ...string) tea.Cmd {
	c := exec.Command(m.exe, args...)
	c.Dir = dir
	c.Env = os.Environ()
	what := "hutch " + strings.Join(args, " ")
	// The command's last words stay on screen until a key.
	wrapped := exec.Command("sh", "-c", `"$@"; code=$?; printf '\n%s ended (exit %s). Press Enter to return to Hutch. ' "$HUTCH_WHAT" "$code"; read -r _; exit $code`, "sh")
	wrapped.Args = append(wrapped.Args, c.Args...)
	wrapped.Dir, wrapped.Env = dir, append(c.Env, "HUTCH_WHAT="+what)
	if _, err := exec.LookPath("sh"); err != nil {
		wrapped = c // Windows without a shell: no pause after
	}
	return tea.ExecProcess(wrapped, func(err error) tea.Msg { return interactiveDoneMsg{what: what, err: err} })
}

func (m *tui) interactiveDone(msg interactiveDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.setStatus(msg.what + ": " + msg.err.Error())
	} else {
		m.setStatus(msg.what + ": done")
	}
	m.resize()
	return m.fetchProjects()
}

func (m *tui) loginMenu() tea.Cmd {
	var items []menuItem
	for _, name := range m.adapters.Names() {
		items = append(items, menuItem{label: name, detail: "hutch login " + name, do: func() tea.Cmd { return m.interactive(m.cwd, "login", name) }})
	}
	items = append(items, menuItem{label: "Every agent here", detail: "hutch login", do: func() tea.Cmd { return m.interactive(m.cwd, "login") }})
	m.choose("Log in: a temporary box opens the agent's own sign-in", items)
	return nil
}

func (m *tui) addMachine() tea.Cmd {
	m.input("Add a machine: its SSH host (it gets hutchd, then joins this lead)", "user@host or a Host from ~/.ssh/config", "", func(host string) tea.Cmd {
		if host == "" {
			return nil
		}
		return m.interactive(m.cwd, "machine", "add", host)
	})
	return nil
}

// removeRow asks before removing a box or a terminal or canceling a task, and
// dismisses a task that ended.
func (m *tui) removeRow(r tuiRow) tea.Cmd {
	if r.kind == rowTask {
		t := r.task
		if t.running() {
			m.ask("Cancel "+t.Title+"?\n\nIt stops here, but the machine may already have done some or all of it.",
				"y cancels · any other key lets it run", func() tea.Cmd {
					m.cancelTask(t)
					return nil
				})
			return nil
		}
		m.tasks = slices.DeleteFunc(m.tasks, func(x *task) bool { return x == t })
		m.build()
		m.sync()
		return nil
	}
	if r.box.ID == "" {
		return nil
	}
	m.confirm = r.key()
	what := "box " + r.box.ID + " and its terminals"
	if r.kind == rowThread {
		what = "terminal " + r.key() + " (" + r.thread.Title + ")"
	}
	m.confirmText = "Remove " + what + "?"
	m.confirmHint = "y removes · any other key keeps it"
	return nil
}

// quit asks first when it would stop running tasks or forwards.
func (m *tui) quit() tea.Cmd {
	running := 0
	for _, t := range m.tasks {
		if t.running() {
			running++
		}
	}
	if running == 0 && len(m.forwards) == 0 {
		return tea.Quit
	}
	var why []string
	if running > 0 {
		why = append(why, fmt.Sprintf("%d running task(s) are canceled (the machine may have done some of their work)", running))
	}
	if len(m.forwards) > 0 {
		why = append(why, fmt.Sprintf("%d forward(s) close", len(m.forwards)))
	}
	m.ask("Quit Hutch?\n\n"+strings.Join(why, "\n"), "y quits · any other key stays", func() tea.Cmd { return tea.Quit })
	return nil
}

// helpView lists the keys: by view, then every action that has one.
func (m *tui) helpView(w, h int) string {
	lines := []string{styleBold.Render("Help") + styleDim.Render("  j/k scroll · esc back"), ""}
	section := func(title string, rows ...string) {
		lines = append(lines, styleLabel.Render(title))
		for _, r := range rows {
			lines = append(lines, "  "+r)
		}
		lines = append(lines, "")
	}
	section("Everywhere",
		"space        action palette: every action, and why one doesn't apply",
		"?            this help",
		"esc          back: closes a view, popup, or search",
		"ctrl+]       while typing into a terminal: give the keys back to Hutch")
	section("The list",
		"j k ↑ ↓      move")
	section("Git (g)",
		"j k          files    enter/l  open the diff    tab  task diff vs base",
		"a / u        stage / unstage the file    A / U  stage / unstage the hunk",
		"[ ]          previous / next hunk        c  commit the staged files",
		"f P p        fetch, pull (fast-forward only), push     r  refresh")
	section("Ports (o)",
		"j k          ports    enter  forward it here (or stop it)",
		"a            forward a port by number    y  copy its address    O  open it")
	section("Tasks (rows at the top of the list)",
		"d            cancel a running task (asked first), or dismiss one that ended")
	lines = append(lines, styleLabel.Render("Keys")+styleDim.Render("  (every other action is in the palette)"))
	for _, a := range m.actions() {
		if a.key != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", a.key, a.name))
		}
	}
	return scrollLines(lines, &m.helpScroll, w, h)
}

// scrollLines shows h lines from *top, kept in range.
func scrollLines(lines []string, top *int, w, h int) string {
	*top = max(min(*top, len(lines)-h), 0)
	var out []string
	for i := *top; i < len(lines) && len(out) < h; i++ {
		out = append(out, truncate(lines[i], w-1))
	}
	return strings.Join(out, "\n")
}

func (m *tui) helpKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		m.helpScroll++
	case "k", "up":
		m.helpScroll--
	case "pgdown", "ctrl+d":
		m.helpScroll += 10
	case "pgup", "ctrl+u":
		m.helpScroll -= 10
	case "esc", "q", "?":
		m.setView(viewNone)
	}
	return nil
}
