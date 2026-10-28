package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/names"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// sidebarWidth is the sidebar's width in columns; tmux keeps it there on resize.
const sidebarWidth = 44

type rowKind int

const (
	rowHeader rowKind = iota // "Boxes ────"
	rowDay                   // TODAY
	rowBlank
	rowBox // a box with several threads, or none
	rowThread
	rowWorker
)

type row struct {
	kind   rowKind
	label  string
	right  string
	box    api.Box
	thread api.Thread
	worker api.Worker
	indent bool
	at     time.Time
}

func (r row) selectable() bool { return r.kind == rowBox || r.kind == rowThread }

type sidebar struct {
	ctx      context.Context
	sess     *yamux.Session
	exe, cwd string
	right    string // the right pane's tmux id
	boxes    []api.Box
	workers  []api.Worker
	rows     []row
	cursor   int
	// showBox and showN are what the right pane shows; showN 0 is a shell.
	showBox     string
	showN       int
	lastThread  int
	input       textinput.Model
	inputFor    string // "new", or a box for a new thread
	pendingOpen string // box/n to open once it appears
	states      map[string]string
	agent       string
	missing     []string // logins the user layer lacks
	warned      bool     // n or t was pressed once with the agent's login missing
	status      string
	err         error
	width       int
	height      int
	frame       int
	offset      int  // first list row on screen
	listTop     int  // screen line of the first list row
	focused     bool // the sidebar pane has focus
	branch      string
	project     string // the launch folder's project, whose boxes need no tag
	pane        string // the sidebar's own tmux pane
	tabStatus   string // the counts last set on this project's tab
}

type boxesMsg struct {
	boxes   []api.Box
	workers []api.Worker
	err     error
	branch  string
	dir     string // where the tab's project turned out to be checked out
}
type tickMsg struct{}
type loginsMsg []string
type doneMsg struct {
	status string
	open   string
}

var (
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleRule    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stylePath    = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styleCursor  = lipgloss.NewStyle().Background(lipgloss.Color("252")).Foreground(lipgloss.Color("235"))
	styleBlurred = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styleWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleWaiting = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	spinner      = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

func runSidebar(ctx context.Context, project string) error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	pane := os.Getenv("TMUX_PANE")
	out, err := tmuxCmd("split-window", "-h", "-d", "-t", pane, "-P", "-F", "#{pane_id}", shellJoin(exe, "tui", "welcome")).Output()
	if err != nil {
		return fmt.Errorf("tmux split-window: %w", err)
	}
	right := strings.TrimSpace(string(out))
	tmuxCmd("resize-pane", "-t", pane, "-x", fmt.Sprint(sidebarWidth)).Run()
	tmuxCmd("select-pane", "-t", pane, "-T", "hutch").Run()
	tmuxCmd("select-pane", "-t", right, "-T", "").Run()
	m := &sidebar{ctx: ctx, exe: exe, cwd: cwd, right: right, states: map[string]string{}, agent: "claude", width: sidebarWidth, focused: true}
	m.project = project
	var here string
	m.branch, here = folderInfo(cwd)
	if here != project || project == "" {
		m.cwd = "" // not checked out here, as far as Hutch knows
	}
	tmuxCmd("set-option", "-w", "-t", pane, "@hutch-key", project).Run()
	m.pane = pane
	m.input = textinput.New()
	m.input.CharLimit = 2000
	m.input.Prompt = "› "
	if m.sess, err = connect(); err != nil {
		return err
	}
	if cfg, err := loadConfig(ctx, m.sess); err == nil {
		m.agent = cfg.String("default_agent")
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion(), tea.WithReportFocus()).Run()
	return err
}

func (m *sidebar) Init() tea.Cmd { return tea.Batch(m.poll(), m.pollLogins(), tick()) }

func (m *sidebar) pollLogins() tea.Cmd {
	sess := m.sess
	return func() tea.Msg {
		var st api.UserState
		if err := rpc.Call(m.ctx, sess, api.UserGet, struct{}{}, &st); err != nil {
			return nil
		}
		missing, err := missingLogins(st)
		if err != nil {
			return nil
		}
		return loginsMsg(missing)
	}
}

// checkedOut reports whether Hutch knows where the tab's project is on this
// machine; new boxes read its settings there.
func (m *sidebar) checkedOut() bool {
	if m.cwd != "" {
		return true
	}
	if m.project == "" {
		m.status = "boxes start in projects: open a project's tab, or hutch init in a repository"
	} else {
		m.status = "Hutch doesn't know where " + path.Base(m.project) + " is on this machine: run hutch once in its checkout"
	}
	return false
}

// agentReady reports whether new threads' agent can start logged in, warning once
// when it can't; pressing the key again goes ahead.
func (m *sidebar) agentReady() bool {
	if m.warned || !slices.Contains(m.missing, m.agent) {
		return true
	}
	m.warned = true
	m.status = m.agent + " isn't logged in, so it would stop at its login: L logs in, or press again to go ahead"
	return false
}

func tick() tea.Cmd { return tea.Tick(time.Second/8, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *sidebar) poll() tea.Cmd {
	sess := m.sess
	return func() tea.Msg {
		boxes, err := listBoxes(m.ctx, sess)
		var workers []api.Worker
		if err == nil {
			err = rpc.Call(m.ctx, sess, api.WorkerList, struct{}{}, &workers)
		}
		branch, dir := "", m.cwd
		if dir == "" && m.project != "" && err == nil {
			dir = projectDir(m.project, boxes)
		}
		if dir != "" {
			branch, _ = folderInfo(dir)
		}
		return boxesMsg{boxes: boxes, workers: workers, err: err, branch: branch, dir: dir}
	}
}

func (m *sidebar) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = msg.Width - 4
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tickMsg:
		m.frame++
		cmds := []tea.Cmd{tick()}
		if m.frame%16 == 0 {
			cmds = append(cmds, m.poll())
		}
		if m.frame%128 == 0 {
			cmds = append(cmds, m.pollLogins())
		}
		return m, tea.Batch(cmds...)
	case boxesMsg:
		if msg.err != nil {
			m.err = msg.err
			if m.sess.IsClosed() {
				if s, err := connect(); err == nil {
					m.sess = s
				}
			}
			return m, nil
		}
		m.err = nil
		m.branch = msg.branch
		if m.cwd == "" && msg.dir != "" {
			m.cwd = msg.dir
		}
		m.notifyChanges(msg.boxes)
		m.boxes, m.workers = msg.boxes, msg.workers
		m.setTabStatus()
		m.build()
		if m.pendingOpen != "" {
			box, n, _ := parseTarget(m.pendingOpen)
			for _, b := range m.boxes {
				for _, t := range b.Threads {
					if b.ID == box && (t.N == n || n == 0) {
						m.pendingOpen = ""
						m.show(box, t.N)
						m.selectThread(box, t.N)
						return m, nil
					}
				}
			}
		}
	case loginsMsg:
		m.missing = msg
		if len(m.missing) == 0 {
			m.warned = false
		}
	case doneMsg:
		m.status = msg.status
		if msg.open != "" {
			m.pendingOpen = msg.open
		}
		return m, tea.Batch(m.poll(), m.pollLogins())
	case tea.KeyMsg:
		if m.inputFor != "" {
			return m.updateInput(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

func (m *sidebar) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.inputFor = ""
		m.input.Blur()
		return m, nil
	case tea.KeyEnter:
		prompt := strings.TrimSpace(m.input.Value())
		target := m.inputFor
		m.inputFor = ""
		m.input.Blur()
		if prompt == "" {
			return m, nil
		}
		if target == "new" {
			taken := map[string]bool{}
			for _, b := range m.boxes {
				taken[b.ID] = true
			}
			name := names.New(taken)
			return m, m.popup("New box", name+"/1", "new", "--name", name, prompt)
		}
		next := 1
		for _, b := range m.boxes {
			if b.ID == target {
				for _, t := range b.Threads {
					next = max(next, t.N+1)
				}
			}
		}
		return m, m.popup("New thread", fmt.Sprintf("%s/%d", target, next), "thread", target, prompt)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *sidebar) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := m.selected()
	box := r.box.ID
	switch msg.String() {
	case "q":
		tmuxCmd("kill-session", "-t", "hutch").Run()
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter", "right", "l":
		if box == "" {
			return m, nil
		}
		n := r.thread.N
		if n == 0 && len(r.box.Threads) > 0 {
			n = r.box.Threads[0].N
		}
		m.show(box, n)
		m.selectThread(box, n)
		tmuxCmd("select-pane", "-t", m.right).Run()
	case "f12":
		if m.showBox == "" {
			return m, nil
		}
		if m.showN == 0 {
			m.show(m.showBox, m.lastThread)
		} else {
			m.show(m.showBox, 0)
		}
		tmuxCmd("select-pane", "-t", m.right).Run()
	case "s":
		if box != "" {
			m.show(box, 0)
			tmuxCmd("select-pane", "-t", m.right).Run()
		}
	case "L":
		if len(m.missing) > 0 {
			return m, m.popup("Log in", "", append([]string{"login"}, m.missing...)...)
		}
		m.status = "every login is in"
	case "n":
		if !m.checkedOut() || !m.agentReady() {
			return m, nil
		}
		m.inputFor = "new"
		m.input.Placeholder = "what should the agent do?"
		m.input.SetValue("")
		return m, m.input.Focus()
	case "t":
		if box != "" && m.checkedOut() && m.agentReady() {
			m.inputFor = box
			m.input.Placeholder = "prompt for a new thread in " + box
			m.input.SetValue("")
			return m, m.input.Focus()
		}
	case "d":
		if box != "" {
			return m, m.popupShell("Diff "+box, shellJoin(m.exe, "diff", box)+" | less -R")
		}
	case "f":
		if box != "" {
			return m, m.popup("Ports of "+box, "", "forward", box)
		}
	case "w":
		return m, m.popup("Workers", "", "workers")
	case "m":
		if box != "" {
			return m, m.popup("Move "+box, "", "move", box)
		}
	case "x":
		if box != "" {
			if m.showBox == box {
				m.respawn(shellJoin(m.exe, "tui", "welcome"))
				m.showBox = ""
			}
			return m, m.popup("Remove "+box, "", "rm", box)
		}
	case "u":
		return m, m.popup("Logins and config", "", "user", "status")
	case "c":
		if box != "" {
			m.respawn(shellJoin(m.exe, "console", box, "--follow"))
			m.showBox = ""
		}
	case "z":
		if box != "" {
			sess := m.sess
			return m, func() tea.Msg {
				if err := rpc.Call(m.ctx, sess, api.BoxSleep, api.IDParams{ID: box}, nil); err != nil {
					return doneMsg{status: err.Error()}
				}
				return doneMsg{status: box + " is asleep"}
			}
		}
	case "p":
		if r.kind == rowThread {
			t := r.thread
			sess := m.sess
			return m, func() tea.Msg {
				rpc.Call(m.ctx, sess, api.ThreadPin, api.ThreadTarget{Box: t.Box, N: t.N, Pinned: !t.Pinned}, nil)
				return doneMsg{}
			}
		}
	case "?":
		return m, m.popupShell("Keys", "printf '%s' "+shellQuote(sidebarHelp)+"; read -rsn1")
	}
	return m, nil
}

const sidebarHelp = `
  enter   show the thread on the right     n   new task in a new box
  s       a shell in the box               t   new thread in the box
  Ctrl-]  sidebar ↔ right pane             d   diff of the box's branch
  Ctrl-\  agent ↔ shell on the right       f   forward the box's ports
  z       sleep the box                    w   workers
  x       remove the box                   m   move the box
  p       pin or unpin the thread          c   serial console
  Ctrl-PgUp/PgDn  previous/next project tab; + on the tab bar opens one
  L       log in to agents and GitHub      u   logins and config
  q       quit (boxes keep running)

  press a key`

// show puts a thread, or with n 0 a shell, of a box in the right pane.
func (m *sidebar) show(box string, n int) {
	target := box
	if n > 0 {
		target = fmt.Sprintf("%s/%d", box, n)
		m.lastThread = n
	}
	m.showBox, m.showN = box, n
	title := "shell · " + box
	for _, b := range m.boxes {
		for _, t := range b.Threads {
			if b.ID == box && t.N == n {
				title = agentLabel(t.Agent) + " · " + target
			}
		}
	}
	tmuxCmd("select-pane", "-t", m.right, "-T", title).Run()
	m.respawn(shellJoin(m.exe, "tui", "connect", target))
}

// inView reports whether the sidebar lists a box: its tab's project's boxes, or
// outside a project the boxes of none.
func (m *sidebar) inView(b api.Box) bool { return b.Project == m.project }

// setTabStatus shows this project's threads on its tab: waiting, working, and
// boxes in trouble (failed, or a rejected push).
func (m *sidebar) setTabStatus() {
	var waiting, working, trouble int
	for _, b := range m.boxes {
		if !m.inView(b) || b.Kind == api.BoxVersion {
			continue
		}
		if b.State == "failed" || b.PushError != "" {
			trouble++
		}
		for _, t := range b.Threads {
			switch t.State {
			case api.ThreadWaiting:
				waiting++
			case api.ThreadWorking:
				working++
			}
		}
	}
	var parts []string
	for _, c := range []struct {
		n     int
		color string
	}{{waiting, "colour214"}, {working, "colour42"}, {trouble, "colour196"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("#[fg=%s]●%d", c.color, c.n))
		}
	}
	status := strings.Join(parts, " ")
	if status != m.tabStatus {
		m.tabStatus = status
		tmuxCmd("set-option", "-w", "-t", m.pane, "@hutch-status", status).Run()
	}
}

// folderInfo is a folder's git branch and Hutch project.
func folderInfo(dir string) (branch, project string) {
	if b, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "-q", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(b))
	}
	if repo, err := config.FindRepo(dir); err == nil && repo != nil {
		project = repo.ID
	}
	return branch, project
}

// agentLabel is how an agent's name reads in titles: claude → Claude.
func agentLabel(a string) string {
	if a == "" {
		return a
	}
	return strings.ToUpper(a[:1]) + a[1:]
}

func (m *sidebar) respawn(cmd string) {
	tmuxCmd("respawn-pane", "-k", "-t", m.right, cmd).Run()
}

// popup runs a hutch command in a tmux popup over the TUI, then opens open.
func (m *sidebar) popup(title, open string, args ...string) tea.Cmd {
	return m.popupShell(title, shellJoin(append([]string{m.exe}, args...)...)+"; printf '\\n[press a key]'; read -rsn1", open)
}

func (m *sidebar) popupShell(title, script string, open ...string) tea.Cmd {
	return func() tea.Msg {
		c := tmuxCmd("display-popup", "-E", "-w", "85%", "-h", "75%", "-d", m.cwd, "-T", " "+title+" ", "sh -c "+shellQuote(script))
		if out, err := c.CombinedOutput(); err != nil {
			return doneMsg{status: strings.TrimSpace(string(out))}
		}
		d := doneMsg{}
		if len(open) > 0 {
			d.open = open[0]
		}
		return d
	}
}

// notifyChanges rings the bell and sends a desktop notification (OSC 9) when a
// thread that was working stops.
func (m *sidebar) notifyChanges(boxes []api.Box) {
	var msgs []string
	next := map[string]string{}
	mine := m.notifiesFor()
	for _, b := range boxes {
		if !mine(b) {
			continue
		}
		for _, t := range b.Threads {
			key := fmt.Sprintf("%s/%d", b.ID, t.N)
			next[key] = t.State
			if m.states[key] == api.ThreadWorking && (t.State == api.ThreadWaiting || t.State == api.ThreadExited) {
				verb := "is waiting"
				if t.State == api.ThreadExited {
					verb = "exited"
				}
				msgs = append(msgs, fmt.Sprintf("%s %s: %s", key, verb, t.Title))
			}
		}
	}
	m.states = next
	if len(msgs) == 0 {
		return
	}
	tty, err := tmuxCmd("display-message", "-p", "#{client_tty}").Output()
	if err != nil {
		return
	}
	f, err := os.OpenFile(strings.TrimSpace(string(tty)), os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	for _, msg := range msgs {
		fmt.Fprintf(f, "\a\033]9;hutch: %s\a", strings.ReplaceAll(msg, "\a", ""))
	}
}

// notifiesFor decides which boxes this tab rings for: its project's, and, in the
// first tab, those of projects with no tab open, so each change rings once.
func (m *sidebar) notifiesFor() func(api.Box) bool {
	open := map[string]bool{}
	first := ""
	out, _ := tmuxCmd("list-windows", "-t", "hutch", "-F", "#{@hutch-key}").Output()
	for _, k := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if first == "" {
			first = k
		}
		open[k] = true
	}
	return func(b api.Box) bool {
		return m.inView(b) || m.project == first && !open[b.Project]
	}
}

// build lays out rows as one tree: pinned threads, then each worker with the
// boxes awake on it, then sleeping boxes, then the template box. A box with one
// thread is that thread's row. Boxes are newest activity first.
func (m *sidebar) build() {
	type entry struct {
		at   time.Time
		rows []row
	}
	var pinned, template []row
	byWorker := map[string][]entry{}
	var asleep, other []entry
	templateState := ""
	for _, b := range m.boxes {
		if b.Kind == api.BoxVersion || !m.inView(b) {
			continue
		}
		at := b.Created
		var threads []api.Thread
		for _, t := range b.Threads {
			if t.Created.After(at) {
				at = t.Created
			}
			if t.Pinned {
				pinned = append(pinned, row{kind: rowThread, box: b, thread: t, label: t.Title, at: t.Created})
				continue
			}
			threads = append(threads, t)
		}
		var rows []row
		if len(threads) == 1 {
			rows = append(rows, row{kind: rowThread, box: b, thread: threads[0], label: threads[0].Title, at: at})
		} else {
			label := b.ID
			if len(threads) > 1 {
				label = fmt.Sprintf("▾ %s (%d threads)", b.ID, len(threads))
			}
			rows = append(rows, row{kind: rowBox, box: b, label: label, at: at})
			for _, t := range threads {
				rows = append(rows, row{kind: rowThread, box: b, thread: t, label: t.Title, indent: true, at: t.Created})
			}
		}
		e := entry{at, rows}
		switch {
		case b.Kind == api.BoxTemplate:
			template = append(template, rows...)
			templateState = boxStateWord(b)
		case b.State == "asleep" || b.State == "archived":
			asleep = append(asleep, e)
		case b.Worker != "":
			byWorker[b.Worker] = append(byWorker[b.Worker], e)
		default:
			other = append(other, e)
		}
	}
	flatten := func(es []entry) []row {
		sort.SliceStable(es, func(i, j int) bool { return es[i].at.After(es[j].at) })
		var out []row
		for _, e := range es {
			out = append(out, e.rows...)
		}
		return out
	}

	selected := m.selected()
	m.rows = nil
	section := func(r row, rows []row) {
		if len(m.rows) > 0 {
			m.rows = append(m.rows, row{kind: rowBlank})
		}
		m.rows = append(m.rows, r)
		m.rows = append(m.rows, rows...)
	}
	if len(pinned) > 0 {
		section(row{kind: rowHeader, label: "Pinned"}, pinned)
	}
	workers := slices.Clone(m.workers)
	sort.SliceStable(workers, func(i, j int) bool {
		if workers[i].Online != workers[j].Online {
			return workers[i].Online
		}
		return workers[i].Name < workers[j].Name
	})
	known := map[string]bool{}
	for _, w := range workers {
		if w.Gone {
			continue
		}
		known[w.ID] = true
		section(row{kind: rowWorker, worker: w}, flatten(byWorker[w.ID]))
	}
	for id, es := range byWorker {
		if !known[id] {
			other = append(other, es...)
		}
	}
	if len(other) > 0 {
		section(row{kind: rowHeader, label: "Waiting"}, flatten(other))
	}
	if len(asleep) > 0 {
		section(row{kind: rowHeader, label: "Asleep"}, flatten(asleep))
	}
	if len(template) > 0 {
		section(row{kind: rowHeader, label: "Template box", right: templateState}, template)
	}
	m.cursor = -1
	for i, r := range m.rows {
		if r.selectable() && r.box.ID == selected.box.ID && r.thread.N == selected.thread.N {
			m.cursor = i
		}
	}
	if m.cursor < 0 {
		m.cursor = 0
		m.move(1)
	}
}

func boxStateWord(b api.Box) string {
	switch b.State {
	case "running":
		return "awake"
	case "":
		return ""
	}
	return b.State
}

func (m *sidebar) selected() row {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return row{}
}

func (m *sidebar) selectThread(box string, n int) {
	for i, r := range m.rows {
		if r.selectable() && r.box.ID == box && r.thread.N == n {
			m.cursor = i
		}
	}
}

func (m *sidebar) move(d int) {
	for i := m.cursor + d; i >= 0 && i < len(m.rows); i += d {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
}

// updateMouse selects and opens a row on click, and moves with the wheel.
func (m *sidebar) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		m.move(-1)
	case msg.Button == tea.MouseButtonWheelDown:
		m.move(1)
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		i := msg.Y - m.listTop + m.offset
		if i >= 0 && i < len(m.rows) && m.rows[i].selectable() && msg.Y >= m.listTop {
			m.cursor = i
			return m.updateKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
	}
	return m, nil
}

func (m *sidebar) View() string {
	w := max(m.width, 20)
	var top []string
	name := "boxes outside projects"
	if m.project != "" {
		name = path.Base(m.project)
	}
	dir := ""
	switch {
	case m.cwd != "":
		dir = m.cwd
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(dir, home) {
			dir = "~" + strings.TrimPrefix(dir, home)
		}
	case m.project != "":
		dir = "not checked out here"
	}
	working := 0
	for _, b := range m.boxes {
		for _, t := range b.Threads {
			if m.inView(b) && t.State == api.ThreadWorking {
				working++
			}
		}
	}
	right := ""
	if working > 0 {
		right = styleWorking.Render(fmt.Sprintf("%d working", working))
	}
	head := " " + stylePath.Render(truncate(name, w-lipgloss.Width(right)-4))
	sub := " " + styleDim.Render(truncate(dir, w-4))
	if m.branch != "" && lipgloss.Width(sub)+len(m.branch)+5 < w {
		sub += styleDim.Render("  ⎇ " + m.branch)
	}
	top = append(top, spread(head, right, w-1), sub, "")
	if len(m.missing) > 0 {
		top = append(top, styleWaiting.Render(truncate(" ! not logged in: "+strings.Join(m.missing, ", "), w-1)),
			styleDim.Render(truncate("   L logs in (a browser opens)", w-1)), "")
	}

	var bottom []string
	wrap := lipgloss.NewStyle().Width(w - 2).PaddingLeft(1)
	switch {
	case m.inputFor != "":
		label := "New task"
		if m.inputFor != "new" {
			label = "New thread in " + m.inputFor
		}
		bottom = append(bottom, styleRule.Render(strings.Repeat("─", w-1)), " "+styleBold.Render(label), " "+m.input.View(), styleDim.Render(" enter starts · esc cancels"))
	case m.err != nil:
		bottom = append(bottom, wrap.Inherit(styleWaiting).Render(truncate(m.err.Error(), w*3)))
	case m.status != "":
		bottom = append(bottom, wrap.Inherit(styleDim).Render(truncate(m.status, w*3)))
	}

	height := m.height - len(top) - len(bottom)
	if height < 3 {
		height = 3
	}
	list := m.rows
	// Keep the cursor on screen, a row of context past it where there is one.
	if m.cursor < m.offset+1 {
		m.offset = max(m.cursor-1, 0)
	}
	if m.cursor > m.offset+height-2 {
		m.offset = m.cursor - height + 2
	}
	m.offset = max(min(m.offset, len(list)-height), 0)
	m.listTop = len(top)

	var b strings.Builder
	for _, l := range top {
		b.WriteString(l + "\n")
	}
	lines := 0
	empty := true
	for _, x := range m.boxes {
		if m.inView(x) && x.Kind != api.BoxVersion {
			empty = false
		}
	}
	if empty {
		b.WriteString(styleDim.Render(" No boxes here yet. n starts a task.") + "\n\n")
		lines += 2
	}
	for i := m.offset; i < len(list) && lines < height; i++ {
		if lines == height-1 && i < len(list)-1 {
			b.WriteString(styleDim.Render(spread("", fmt.Sprintf("↓ %d more", len(list)-i), w-1)) + "\n")
			lines++
			break
		}
		line := m.renderRow(list[i], w-1)
		if i == m.cursor && list[i].selectable() {
			st := styleCursor
			if !m.focused {
				st = styleBlurred
			}
			line = st.Render(padRight(lipgloss.NewStyle().UnsetForeground().Render(stripStyle(line)), w-1))
		}
		b.WriteString(line + "\n")
		lines++
	}
	for ; lines < height; lines++ {
		b.WriteString("\n")
	}
	for _, l := range bottom {
		b.WriteString(l + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m *sidebar) renderRow(r row, w int) string {
	switch r.kind {
	case rowBlank:
		return ""
	case rowDay:
		return " " + styleDim.Render(r.label)
	case rowHeader:
		left := " " + styleBold.Render(r.label) + " "
		right := ""
		if r.right != "" {
			right = " " + styleDim.Render(r.right)
		}
		fill := w - lipgloss.Width(left) - lipgloss.Width(right)
		return left + styleRule.Render(strings.Repeat("─", max(fill, 0))) + right
	}
	if r.kind == rowWorker {
		return m.renderWorker(r.worker, w)
	}
	b := r.box
	bullet := styleDim.Render("·")
	switch {
	case b.Merged != "":
		bullet = styleWorking.Render("✓")
	case b.State == "asleep":
		bullet = styleDim.Render("z")
	case b.State == "archived":
		bullet = styleDim.Render("▫")
	case b.State == "queued":
		bullet = styleDim.Render("⏸")
	case r.kind == rowBox:
		bullet = ""
	case r.thread.State == api.ThreadWorking:
		bullet = styleWorking.Render(spinner[m.frame%len(spinner)])
	case r.thread.State == api.ThreadWaiting:
		bullet = styleWaiting.Render("●")
	}
	indent := " "
	if r.indent {
		indent = "   "
	}
	label := r.label
	if r.kind == rowThread && !r.indent {
		label = b.ID + " · " + label
	}
	tag := ""
	if r.kind == rowThread && r.thread.Agent != m.agent {
		tag = " [" + r.thread.Agent + "]"
	}
	if !r.indent && b.Project != "" && b.Project != m.project {
		tag += " · " + path.Base(b.Project)
	}
	age := shortAge(r.at)
	left := indent
	if bullet != "" {
		left += bullet + " "
	}
	room := w - lipgloss.Width(left) - len([]rune(tag)) - len(age) - 2
	text := truncate(label, room)
	shown := b.ID == m.showBox && (r.kind == rowThread && r.thread.N == m.showN || r.kind == rowBox && m.showN == 0)
	switch {
	case shown:
		text = styleBold.Render(text)
	case r.kind == rowBox:
		text = styleDim.Render(text)
	}
	return spread(left+text+styleDim.Render(tag), styleDim.Render(age), w)
}

// renderWorker is a worker's header: state, name, a rule, and the memory its
// awake boxes reserve, or that it's offline.
func (m *sidebar) renderWorker(wk api.Worker, w int) string {
	bullet, detail := styleWaiting.Render("!"), "offline"
	if wk.Online {
		bullet = styleWorking.Render("●")
		detail = fmt.Sprintf("%d/%d GB", (wk.ReservedMiB+512)/1024, (wk.MemMiB+512)/1024)
		if wk.Draining {
			detail = "draining " + detail
		}
	}
	left := " " + bullet + " " + styleBold.Render(truncate(wk.Name, w/2)) + " "
	right := " " + styleDim.Render(detail)
	fill := w - lipgloss.Width(left) - lipgloss.Width(right)
	return left + styleRule.Render(strings.Repeat("─", max(fill, 0))) + right
}

// spread puts right at the right edge of a line of width w.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// shortAge is how long ago, in at most three characters: now, 12m, 4h, 6d, 3w.
func shortAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return ""
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dw", int(d.Hours()/24/7))
}

// stripStyle removes styling, so a selected row takes the selection's colors.
func stripStyle(s string) string { return ansi.Strip(s) }

func truncate(s string, n int) string {
	r := []rune(s)
	if n < 1 {
		return ""
	}
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
