package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/names"
	"github.com/mouizahmed/hutch/internal/rpc"
)

const sidebarWidth = 40

type rowKind int

const (
	rowSection rowKind = iota
	rowBox
	rowThread
)

type row struct {
	kind   rowKind
	label  string
	box    api.Box
	thread api.Thread
	indent bool
}

type sidebar struct {
	ctx      context.Context
	sess     *yamux.Session
	exe, cwd string
	right    string // the right pane's tmux id
	boxes    []api.Box
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
	height      int
	frame       int
}

type boxesMsg struct {
	boxes []api.Box
	err   error
}
type tickMsg struct{}
type loginsMsg []string
type doneMsg struct {
	status string
	open   string
}

var (
	styleSection = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleCursor  = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	styleShown   = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styleWaiting = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleMerged  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	spinner      = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

func runSidebar(ctx context.Context) error {
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
	tmuxCmd("resize-pane", "-t", pane, "-x", fmt.Sprint(sidebarWidth)).Run()
	m := &sidebar{ctx: ctx, exe: exe, cwd: cwd, right: strings.TrimSpace(string(out)), states: map[string]string{}, agent: "claude"}
	m.input = textinput.New()
	m.input.CharLimit = 2000
	m.input.Width = sidebarWidth - 4
	if m.sess, err = connect(); err != nil {
		return err
	}
	if cfg, err := loadConfig(ctx, m.sess); err == nil {
		m.agent = cfg.String("default_agent")
		// Opening the TUI may start a worker early (providers.prestart), so the first
		// box doesn't wait for one.
		go rpc.Call(ctx, m.sess, api.Prestart, api.PrestartParams{VCPUs: int(cfg.Int("size.cpu")), MemMiB: int(cfg.Size("size.memory") >> 20)}, nil)
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
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
		return boxesMsg{boxes, err}
	}
}

func (m *sidebar) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
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
		m.notifyChanges(msg.boxes)
		m.boxes = msg.boxes
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
		if !m.agentReady() {
			return m, nil
		}
		m.inputFor = "new"
		m.input.Placeholder = "what should the agent do?"
		m.input.SetValue("")
		return m, m.input.Focus()
	case "t":
		if box != "" && m.agentReady() {
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
	m.respawn(shellJoin(m.exe, "tui", "connect", target))
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
	for _, b := range boxes {
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

// build lays out rows: pinned threads, then boxes by last activity in day
// sections, then template boxes. A box with one thread is that thread's row.
func (m *sidebar) build() {
	var pinned []row
	type entry struct {
		at   time.Time
		rows []row
	}
	var entries []entry
	var template []row
	for _, b := range m.boxes {
		if b.Kind == api.BoxVersion {
			continue
		}
		at := b.Created
		var rows []row
		var threads []api.Thread
		for _, t := range b.Threads {
			if t.Created.After(at) {
				at = t.Created
			}
			if t.Pinned {
				pinned = append(pinned, row{kind: rowThread, box: b, thread: t, label: t.Title})
				continue
			}
			threads = append(threads, t)
		}
		switch {
		case len(threads) == 1:
			rows = append(rows, row{kind: rowThread, box: b, thread: threads[0], label: threads[0].Title})
		default:
			label := b.ID
			if len(threads) > 1 {
				label = fmt.Sprintf("▾ %s (%d threads)", b.ID, len(threads))
			}
			rows = append(rows, row{kind: rowBox, box: b, label: label})
			for _, t := range threads {
				rows = append(rows, row{kind: rowThread, box: b, thread: t, label: t.Title, indent: true})
			}
		}
		if b.Kind == api.BoxTemplate {
			template = append(template, rows...)
			continue
		}
		entries = append(entries, entry{at, rows})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.After(entries[j].at) })
	selected := m.selected()
	m.rows = nil
	if len(pinned) > 0 {
		m.rows = append(m.rows, row{kind: rowSection, label: "PINNED"})
		m.rows = append(m.rows, pinned...)
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	section := ""
	for _, e := range entries {
		s := "OLDER"
		switch {
		case !e.at.Before(today):
			s = "TODAY"
		case !e.at.Before(today.AddDate(0, 0, -1)):
			s = "YESTERDAY"
		}
		if s != section {
			section = s
			m.rows = append(m.rows, row{kind: rowSection, label: s})
		}
		m.rows = append(m.rows, e.rows...)
	}
	if len(template) > 0 {
		m.rows = append(m.rows, row{kind: rowSection, label: "TEMPLATE"})
		m.rows = append(m.rows, template...)
	}
	m.cursor = 0
	for i, r := range m.rows {
		if r.kind != rowSection && r.box.ID == selected.box.ID && r.thread.N == selected.thread.N {
			m.cursor = i
		}
	}
	if m.cursor == 0 {
		m.move(1)
	}
}

func (m *sidebar) selected() row {
	if m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return row{}
}

func (m *sidebar) selectThread(box string, n int) {
	for i, r := range m.rows {
		if r.box.ID == box && r.thread.N == n {
			m.cursor = i
		}
	}
}

func (m *sidebar) move(d int) {
	for i := m.cursor + d; i >= 0 && i < len(m.rows); i += d {
		if m.rows[i].kind != rowSection {
			m.cursor = i
			return
		}
	}
}

func (m *sidebar) View() string {
	var b strings.Builder
	title := " hutch"
	working := 0
	for _, x := range m.boxes {
		for _, t := range x.Threads {
			if t.State == api.ThreadWorking {
				working++
			}
		}
	}
	if working > 0 {
		title += fmt.Sprintf(" · %d working", working)
	}
	b.WriteString(styleSection.Render(title) + "\n")
	if len(m.missing) > 0 {
		b.WriteString(styleWaiting.Render(" ! not logged in: "+strings.Join(m.missing, ", ")) + "\n")
		b.WriteString(styleDim.Render("   L logs in (a browser opens)") + "\n")
	}
	b.WriteString("\n")
	lines := 0
	for i, r := range m.rows {
		line := m.renderRow(r)
		if i == m.cursor {
			line = styleCursor.Render(padRight(line, sidebarWidth-1))
		}
		b.WriteString(line + "\n")
		lines++
	}
	if len(m.rows) == 0 && m.err == nil {
		b.WriteString(styleDim.Render(" No boxes yet: n starts a task.") + "\n")
	}
	b.WriteString("\n")
	if m.inputFor != "" {
		b.WriteString(" " + m.input.View() + "\n" + styleDim.Render(" enter start · esc cancel") + "\n")
	} else {
		wrap := lipgloss.NewStyle().Width(sidebarWidth - 2).PaddingLeft(1)
		if m.err != nil {
			b.WriteString(wrap.Inherit(styleWaiting).Render(truncate(m.err.Error(), sidebarWidth*3)) + "\n")
		} else if m.status != "" {
			b.WriteString(wrap.Inherit(styleDim).Render(truncate(m.status, sidebarWidth*3)) + "\n")
		}
		b.WriteString(styleDim.Render(" enter open · n new · t thread · ? keys") + "\n")
	}
	return b.String()
}

func (m *sidebar) renderRow(r row) string {
	if r.kind == rowSection {
		return " " + styleSection.Render(r.label)
	}
	b := r.box
	mark := ""
	switch {
	case b.Merged != "":
		mark = styleMerged.Render("✓")
	case b.State == "asleep":
		mark = styleDim.Render("zz")
	case b.State == "archived":
		mark = styleDim.Render("▫")
	case b.State == "queued":
		mark = styleDim.Render("⏸")
	case r.kind == rowThread && r.thread.State == api.ThreadWorking:
		mark = spinner[m.frame%len(spinner)]
	case r.kind == rowThread && r.thread.State == api.ThreadWaiting:
		mark = styleWaiting.Render("●")
	case r.kind == rowThread && r.thread.State == api.ThreadExited:
		mark = styleDim.Render("×")
	}
	label := r.label
	if r.kind == rowThread && !r.indent {
		label = b.ID + " · " + label
	}
	tag := ""
	if r.kind == rowThread && r.thread.Agent != m.agent {
		tag = " [" + r.thread.Agent + "]"
	}
	indent := " "
	if r.indent {
		indent = "   "
	}
	room := sidebarWidth - 1 - len([]rune(indent)) - lipgloss.Width(mark) - lipgloss.Width(tag) - 2
	text := indent + truncate(label, room) + styleDim.Render(tag)
	if r.box.ID == m.showBox && (r.kind == rowThread && r.thread.N == m.showN || r.kind == rowBox && m.showN == 0) {
		text = indent + styleShown.Render(truncate(label, room)) + styleDim.Render(tag)
	}
	return padRight(text, sidebarWidth-2-lipgloss.Width(mark)) + mark
}

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
