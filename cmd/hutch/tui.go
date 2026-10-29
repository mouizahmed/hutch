package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/names"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The TUI is one program: boxes and their threads on the left, the selected
// thread's terminal on the right, drawn through an in-process emulator. Anything
// it doesn't do itself runs as the CLI command, in the terminal, and comes back.

type rowKind int

const (
	rowProject rowKind = iota
	rowBox
	rowThread
	rowShell
)

type tuiRow struct {
	kind   rowKind
	label  string
	box    api.Box
	thread api.Thread
}

func (r tuiRow) selectable() bool { return r.kind != rowProject }

// key identifies a row across refreshes.
func (r tuiRow) key() string {
	switch r.kind {
	case rowThread:
		return paneKey(r.box.ID, r.thread.N)
	case rowShell:
		return paneKey(r.box.ID, 0)
	case rowBox:
		return r.box.ID
	}
	return "project " + r.label
}

type tui struct {
	ctx      context.Context
	sess     *yamux.Session
	prog     *tea.Program
	exe, cwd string
	here     string // the launch folder's project

	listWidth, kept int
	refresh         time.Duration

	boxes    []api.Box
	workers  []api.Worker
	reached  bool              // the last refresh reached the machine
	projects map[string]string // id → slug
	rows     []tuiRow
	cursor   int
	offset   int
	shells   map[string]bool // boxes with a shell row

	panes map[string]*pane
	order []string // pane keys, most recently shown last
	shown atomic.Value
	dirty atomic.Bool

	typing, zoom, below, focused bool
	w, h                         int
	frame                        int

	input    textinput.Model
	inputFor string // "new", or the box a new thread goes in
	confirm  string // the row key d asks about
	open     string // box/n to select once it appears
	status   string
	statusAt time.Time

	states map[string]string // thread states at the last refresh, for notifications
	read   map[string]bool   // threads you've looked at since they last worked
}

type boxesMsg struct {
	boxes    []api.Box
	projects []api.Project
	workers  []api.Worker
	err      error
}
type (
	frameMsg   struct{}
	refreshMsg struct{}
	paneMsg    struct{}
	clipMsg    string
	doneMsg    struct{ status, open string }
)

var (
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleRule    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleCursor  = lipgloss.NewStyle().Background(lipgloss.Color("252")).Foreground(lipgloss.Color("235"))
	styleBlurred = lipgloss.NewStyle().Background(lipgloss.Color("238"))
	styleTyping  = lipgloss.NewStyle().Background(lipgloss.Color("42")).Foreground(lipgloss.Color("235"))
	styleWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleUnread  = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	styleRead    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleExited  = lipgloss.NewStyle().Foreground(lipgloss.Color("170"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	spinner      = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

func runTUI(cmd *cobra.Command, _ []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return cmd.Help()
	}
	sess, err := connect()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	cfg, err := loadConfig(ctx, sess)
	if err != nil {
		sess.Close()
		return err
	}
	exe, err := selfPath()
	if err != nil {
		sess.Close()
		return err
	}
	cwd, _ := os.Getwd()
	m := &tui{ctx: ctx, sess: sess, exe: exe, cwd: cwd, focused: true, w: 120, h: 40,
		listWidth: int(cfg.Int("tui.list_width")), kept: int(cfg.Int("tui.kept_panes")), refresh: cfg.Duration("tui.refresh"),
		projects: map[string]string{}, shells: map[string]bool{}, panes: map[string]*pane{},
		states: map[string]string{}, read: map[string]bool{}}
	if repo, _ := config.FindRepo(cwd); repo != nil {
		m.here = repo.ID
	}
	m.shown.Store("")
	m.input = textinput.New()
	m.input.CharLimit = 4000
	m.input.Prompt = ""
	m.prog = tea.NewProgram(m, tea.WithContext(ctx))
	_, err = m.prog.Run()
	for _, p := range m.panes {
		p.close()
	}
	m.sess.Close()
	return err
}

func (m *tui) Init() tea.Cmd { return tea.Batch(m.poll(), frame()) }

func frame() tea.Cmd { return tea.Tick(time.Second/8, func(time.Time) tea.Msg { return frameMsg{} }) }

func (m *tui) poll() tea.Cmd {
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		var msg boxesMsg
		if msg.err = rpc.Call(ctx, sess, api.BoxList, struct{}{}, &msg.boxes); msg.err == nil {
			msg.err = rpc.Call(ctx, sess, api.ProjectList, struct{}{}, &msg.projects)
		}
		if msg.err == nil {
			msg.err = rpc.Call(ctx, sess, api.WorkerList, struct{}{}, &msg.workers)
		}
		return msg
	}
}

// layout is where the pane goes: beside the list, below it, or the whole screen
// but the footer.
func (m *tui) layout() (x, y, w, h int) {
	switch {
	case m.zoom:
		return 0, 0, m.w, m.h - 1
	case m.below:
		lh := min(len(m.rows)+2, m.h/3)
		return 0, lh + 1, m.w, max(m.h-lh-2, 1)
	}
	lw := min(m.listWidth, m.w/2)
	return lw + 1, 0, max(m.w-lw-1, 1), m.h - 1
}

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resize()
	case tea.FocusMsg:
		m.focused = true
		m.markHidden()
	case tea.BlurMsg:
		m.focused = false
		m.markHidden()
	case frameMsg:
		m.frame++
		if m.status != "" && time.Since(m.statusAt) > 8*time.Second && m.confirm == "" {
			m.status = ""
		}
		return m, frame()
	case refreshMsg:
		return m, m.poll()
	case paneMsg:
		m.dirty.Store(false)
	case clipMsg:
		return m, tea.SetClipboard(string(msg))
	case boxesMsg:
		next := tea.Tick(m.refresh, func(time.Time) tea.Msg { return refreshMsg{} })
		m.reached = msg.err == nil
		if msg.err != nil {
			m.setStatus("can't reach the machine: " + msg.err.Error())
			if m.sess.IsClosed() {
				if s, err := connect(); err == nil {
					m.sess = s
					for k, p := range m.panes {
						p.close()
						delete(m.panes, k)
					}
					m.order = nil
				}
			}
			return m, next
		}
		notes := m.changes(msg.boxes)
		m.boxes, m.workers = msg.boxes, msg.workers
		for _, p := range msg.projects {
			m.projects[p.ID] = p.Slug
		}
		m.build()
		if m.open != "" {
			if m.selectKey(m.open) {
				m.open = ""
			}
		}
		m.sync()
		return m, tea.Batch(next, notes)
	case doneMsg:
		if msg.status != "" {
			m.setStatus(msg.status)
		}
		if msg.open != "" {
			m.open = msg.open
		}
		return m, m.poll()
	case tea.PasteMsg:
		if m.inputFor != "" {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		if p := m.shownPane(); m.typing && p != nil {
			p.withEmu(func(e *vt.Emulator) { e.Paste(msg.Content) })
		}
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyPressMsg:
		switch {
		case m.inputFor != "":
			return m, m.inputKey(msg)
		case m.confirm != "":
			return m, m.confirmKey(msg)
		case m.typing:
			if msg.String() == "ctrl+]" {
				m.typing = false
				return m, nil
			}
			if p := m.shownPane(); p != nil {
				k := uv.KeyPressEvent(uv.Key(msg))
				p.withEmu(func(e *vt.Emulator) { e.SendKey(k) })
			}
			return m, nil
		}
		return m, m.key(msg)
	}
	return m, nil
}

func (m *tui) setStatus(s string) { m.status, m.statusAt = s, time.Now() }

func (m *tui) key(msg tea.KeyPressMsg) tea.Cmd {
	r := m.selected()
	switch msg.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "j", "down":
		m.move(1)
	case "k", "up":
		m.move(-1)
	case "enter":
		return m.enter(r)
	case "n":
		if m.here == "" {
			m.setStatus("new tasks start in a project: run hutch in a project's checkout")
			return nil
		}
		return m.ask("new", "what should the agent do?")
	case "N":
		if r.box.ID != "" {
			return m.ask(r.box.ID, "what should the new thread in "+r.box.ID+" do?")
		}
	case "t":
		if r.box.ID != "" {
			m.shells[r.box.ID] = true
			m.build()
			m.selectKey(paneKey(r.box.ID, 0))
			m.sync()
		}
	case "s":
		if r.box.ID != "" {
			return m.sleepWake(r.box)
		}
	case "d":
		if r.kind != rowProject && r.box.ID != "" {
			m.confirm = r.key()
			what := "box " + r.box.ID + " and its threads"
			switch r.kind {
			case rowThread:
				what = "thread " + r.key() + " (" + r.thread.Title + ")"
			case rowShell:
				what = r.box.ID + "'s shell"
			}
			m.setStatus("Remove " + what + "? y removes, any other key keeps it")
		}
	case "\\":
		m.below = !m.below
		m.resize()
	case "z":
		m.zoom = !m.zoom
		m.resize()
	}
	return nil
}

// enter wakes a sleeping box (saying so), reconnects a dropped terminal, or types
// into a live one.
func (m *tui) enter(r tuiRow) tea.Cmd {
	if r.box.ID == "" {
		return nil
	}
	if r.box.State != "running" {
		return m.sleepWake(r.box)
	}
	p := m.shownPane()
	switch {
	case p == nil:
		return nil
	case p.ended.Load():
		m.drop(m.shown.Load().(string))
		m.sync()
	default:
		m.typing = true
	}
	return nil
}

func (m *tui) sleepWake(b api.Box) tea.Cmd {
	ctx, sess := m.ctx, m.sess
	method, doing, done := api.BoxSleep, "putting "+b.ID+" to sleep…", b.ID+" is asleep"
	if b.State != "running" {
		method, doing, done = api.BoxWake, "waking "+b.ID+"…", b.ID+" is awake"
	}
	m.setStatus(doing)
	return func() tea.Msg {
		if err := rpc.Call(ctx, sess, method, api.IDParams{ID: b.ID}, nil); err != nil {
			return doneMsg{status: err.Error()}
		}
		return doneMsg{status: done}
	}
}

func (m *tui) ask(target, placeholder string) tea.Cmd {
	m.inputFor = target
	m.input.Placeholder = placeholder
	m.input.SetValue("")
	m.input.SetWidth(max(m.w-30, 20))
	return m.input.Focus()
}

func (m *tui) inputKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.inputFor = ""
		m.input.Blur()
		return nil
	case "enter":
		prompt := strings.TrimSpace(m.input.Value())
		target := m.inputFor
		m.inputFor = ""
		m.input.Blur()
		if prompt == "" {
			return nil
		}
		if target == "new" {
			taken := map[string]bool{}
			for _, b := range m.boxes {
				taken[b.ID] = true
			}
			name := names.New(taken)
			return m.run(m.cwd, name+"/1", "new", "--name", name, prompt)
		}
		next := 1
		for _, b := range m.boxes {
			if b.ID == target {
				for _, t := range b.Threads {
					next = max(next, t.N+1)
				}
			}
		}
		dir := m.cwd
		for _, b := range m.boxes {
			if b.ID == target && b.Project != "" && b.Project != m.here {
				if d := projectDir(b.Project, m.boxes); d != "" {
					dir = d
				}
			}
		}
		return m.run(dir, paneKey(target, next), "thread", target, prompt)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *tui) confirmKey(msg tea.KeyPressMsg) tea.Cmd {
	key := m.confirm
	m.confirm = ""
	m.status = ""
	if msg.String() != "y" {
		return nil
	}
	for _, r := range m.rows {
		if r.key() != key {
			continue
		}
		switch r.kind {
		case rowShell:
			delete(m.shells, r.box.ID)
			m.drop(key)
			m.build()
			m.sync()
			return nil
		case rowThread:
			m.drop(key)
			return m.run(m.cwd, "", "thread", "rm", key)
		default:
			for k, p := range m.panes {
				if p.box == r.box.ID {
					m.drop(k)
				}
			}
			delete(m.shells, r.box.ID)
			return m.run(m.cwd, "", "rm", r.box.ID)
		}
	}
	return nil
}

// run runs a hutch command in the terminal, in dir, then comes back to the TUI,
// pausing first if it failed so its error can be read.
func (m *tui) run(dir, open string, args ...string) tea.Cmd {
	c := exec.Command("sh", "-c", shellJoin(append([]string{m.exe}, args...)...)+` || { printf '\n[press Enter]'; read _; }`)
	c.Dir = dir
	return tea.ExecProcess(c, func(err error) tea.Msg {
		if err != nil {
			return doneMsg{status: err.Error()}
		}
		return doneMsg{open: open}
	})
}

func (m *tui) projectName(id string) string {
	if name := m.projects[id]; name != "" {
		return name
	}
	return path.Base(id)
}

func (m *tui) mouse(msg tea.MouseMsg) tea.Cmd {
	mo := msg.Mouse()
	px, py, pw, ph := m.layout()
	inPane := mo.X >= px && mo.X < px+pw && mo.Y >= py && mo.Y < py+ph
	if p := m.shownPane(); inPane && p != nil && !p.ended.Load() {
		if _, click := msg.(tea.MouseClickMsg); click && !m.typing {
			m.typing = true
		}
		e := uv.Mouse{X: mo.X - px, Y: mo.Y - py, Button: mo.Button, Mod: mo.Mod}
		var ev vt.Mouse
		switch msg.(type) {
		case tea.MouseClickMsg:
			ev = uv.MouseClickEvent(e)
		case tea.MouseReleaseMsg:
			ev = uv.MouseReleaseEvent(e)
		case tea.MouseWheelMsg:
			ev = uv.MouseWheelEvent(e)
		case tea.MouseMotionMsg:
			ev = uv.MouseMotionEvent(e)
		}
		if ev != nil {
			p.withEmu(func(em *vt.Emulator) { em.SendMouse(ev) })
		}
		return nil
	}
	if m.zoom || inPane {
		return nil
	}
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			m.move(-1)
		} else if msg.Button == tea.MouseWheelDown {
			m.move(1)
		}
	case tea.MouseClickMsg:
		m.typing = false
		if i := m.offset + mo.Y - 1; mo.Y > 0 && i < len(m.rows) && m.rows[i].selectable() {
			m.cursor = i
			m.sync()
		}
	}
	return nil
}

// changes notices threads that stopped working, and rings for those you aren't
// looking at: the bell, and a desktop notification (OSC 9).
func (m *tui) changes(boxes []api.Box) tea.Cmd {
	var out strings.Builder
	next := map[string]string{}
	for _, b := range boxes {
		for _, t := range b.Threads {
			key := paneKey(b.ID, t.N)
			next[key] = t.State
			if t.State == api.ThreadWorking {
				delete(m.read, key)
			}
			if m.states[key] != api.ThreadWorking || t.State == api.ThreadWorking || m.looking(key) {
				continue
			}
			verb := "is waiting"
			if t.State == api.ThreadExited {
				verb = "exited"
			}
			fmt.Fprintf(&out, "\a\x1b]9;hutch: %s %s: %s\a", key, verb, strings.ReplaceAll(t.Title, "\a", ""))
		}
	}
	m.states = next
	if out.Len() == 0 {
		return nil
	}
	return tea.Raw(out.String())
}

func (m *tui) looking(key string) bool { return m.focused && m.shown.Load().(string) == key }

// build lays out the rows: each project, the launch folder's first, then its
// boxes, most recently started first, with their threads and shell; template
// boxes last.
func (m *tui) build() {
	selected := m.selected().key()
	type group struct {
		id    string
		at    time.Time
		boxes []api.Box
	}
	groups := map[string]*group{}
	for _, b := range m.boxes {
		if b.Kind == api.BoxVersion {
			continue
		}
		g := groups[b.Project]
		if g == nil {
			g = &group{id: b.Project}
			groups[b.Project] = g
		}
		g.boxes = append(g.boxes, b)
		if at := boxActive(b); at.After(g.at) {
			g.at = at
		}
	}
	var order []*group
	for _, g := range groups {
		order = append(order, g)
	}
	sort.Slice(order, func(i, j int) bool {
		if (order[i].id == m.here) != (order[j].id == m.here) {
			return order[i].id == m.here
		}
		return order[i].at.After(order[j].at)
	})
	m.rows = nil
	for _, g := range order {
		name := m.projectName(g.id)
		if g.id == "" {
			name = "outside projects"
		}
		m.rows = append(m.rows, tuiRow{kind: rowProject, label: name})
		sort.SliceStable(g.boxes, func(i, j int) bool {
			if (g.boxes[i].Kind == api.BoxTemplate) != (g.boxes[j].Kind == api.BoxTemplate) {
				return g.boxes[j].Kind == api.BoxTemplate
			}
			return boxActive(g.boxes[i]).After(boxActive(g.boxes[j]))
		})
		for _, b := range g.boxes {
			label := b.ID
			if b.Kind == api.BoxTemplate {
				label = "template"
			}
			m.rows = append(m.rows, tuiRow{kind: rowBox, box: b, label: label})
			for _, t := range b.Threads {
				m.rows = append(m.rows, tuiRow{kind: rowThread, box: b, thread: t, label: t.Title})
			}
			if m.shells[b.ID] {
				m.rows = append(m.rows, tuiRow{kind: rowShell, box: b, label: "shell"})
			}
		}
	}
	if !m.selectKey(selected) {
		m.cursor = min(m.cursor, len(m.rows)-1)
		if m.cursor < 0 || !m.rows[m.cursor].selectable() {
			m.cursor = 0
			m.move(1)
		}
	}
}

func boxActive(b api.Box) time.Time {
	at := b.Created
	for _, t := range b.Threads {
		if t.Created.After(at) {
			at = t.Created
		}
	}
	return at
}

func (m *tui) selected() tuiRow {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return m.rows[m.cursor]
	}
	return tuiRow{}
}

func (m *tui) selectKey(key string) bool {
	for i, r := range m.rows {
		if r.selectable() && r.key() == key {
			m.cursor = i
			return true
		}
	}
	return false
}

func (m *tui) move(d int) {
	for i := m.cursor + d; i >= 0 && i < len(m.rows); i += d {
		if m.rows[i].selectable() {
			m.cursor = i
			m.typing = false
			m.sync()
			return
		}
	}
}

// target is the terminal a row shows: its thread, its shell, or for a box its
// latest thread. A box that isn't running shows none: nothing wakes by browsing.
func (m *tui) target(r tuiRow) (box string, n int, ok bool) {
	if r.box.State != "running" {
		return "", 0, false
	}
	switch r.kind {
	case rowThread:
		return r.box.ID, r.thread.N, true
	case rowShell:
		return r.box.ID, 0, true
	case rowBox:
		if len(r.box.Threads) > 0 {
			return r.box.ID, r.box.Threads[len(r.box.Threads)-1].N, true
		}
	}
	return "", 0, false
}

// sync shows the selected row's terminal, connecting it if it isn't kept, and
// drops terminals of boxes that stopped and those past tui.kept_panes.
func (m *tui) sync() {
	running := map[string]bool{}
	for _, b := range m.boxes {
		running[b.ID] = b.State == "running"
	}
	for k, p := range m.panes {
		if !running[p.box] {
			m.drop(k)
		}
	}
	key := ""
	if box, n, ok := m.target(m.selected()); ok {
		key = paneKey(box, n)
		if m.panes[key] == nil {
			m.connect(box, n)
		}
		m.order = append(slices.DeleteFunc(m.order, func(k string) bool { return k == key }), key)
		for len(m.order) > m.kept {
			m.drop(m.order[0])
		}
		if n > 0 && m.focused {
			m.read[key] = true
		}
	}
	if m.shown.Load().(string) != key {
		m.typing = false
	}
	m.shown.Store(key)
	m.markHidden()
}

func (m *tui) connect(box string, n int) {
	_, _, w, h := m.layout()
	p := newPane(box, n, w, h)
	key := paneKey(box, n)
	m.panes[key] = p
	dir := "/workspace"
	for _, b := range m.boxes {
		if b.ID == box && b.Path != "" {
			dir = b.Path
		}
	}
	changed := func() {
		if m.shown.Load().(string) == key && m.dirty.CompareAndSwap(false, true) {
			go m.prog.Send(paneMsg{})
		}
	}
	go p.run(m.ctx, m.sess, dir, changed, func(s string) { go m.prog.Send(clipMsg(s)) })
}

func (m *tui) drop(key string) {
	if p := m.panes[key]; p != nil {
		p.close()
		delete(m.panes, key)
	}
	m.order = slices.DeleteFunc(m.order, func(k string) bool { return k == key })
}

// markHidden tells each kept thread terminal whether it's on screen, for unread.
func (m *tui) markHidden() {
	for key, p := range m.panes {
		p.setHidden(m.ctx, m.sess, !m.looking(key))
	}
}

func (m *tui) shownPane() *pane { return m.panes[m.shown.Load().(string)] }

func (m *tui) resize() {
	_, _, w, h := m.layout()
	for _, p := range m.panes {
		p.resize(w, h)
	}
}

func (m *tui) View() tea.View {
	px, py, pw, ph := m.layout()
	screen, cursor := m.paneView(pw, ph)
	var body string
	switch {
	case m.zoom:
		body = screen
	case m.below:
		lh := py - 1
		body = lipgloss.JoinVertical(lipgloss.Left, m.listView(m.w, lh), styleRule.Render(strings.Repeat("─", m.w)), screen)
	default:
		sep := styleRule.Render(strings.TrimSuffix(strings.Repeat("│\n", ph), "\n"))
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listView(px-1, ph), sep, screen)
	}
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, body, m.footer()))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = true
	v.WindowTitle = "hutch"
	if m.typing && cursor != nil {
		v.Cursor = tea.NewCursor(px+cursor.X, py+cursor.Y)
	}
	return v
}

func (m *tui) paneView(w, h int) (string, *uv.Position) {
	box := lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h)
	r := m.selected()
	p := m.shownPane()
	switch {
	case len(m.rows) == 0:
		return box.Render(styleDim.Render("\n  No boxes yet. n starts a task in a new box.")), nil
	case p != nil && p.ended.Load():
		why, _ := p.err.Load().(string)
		return box.Render(styleDim.Render(fmt.Sprintf("\n  %s: %s. Enter reconnects.", m.shown.Load(), why))), nil
	case p != nil:
		s, cur := p.render()
		return box.Render(s), cur
	case r.box.ID != "" && r.box.State != "running":
		return box.Render(styleDim.Render(fmt.Sprintf("\n  %s is %s. Enter wakes it.", r.box.ID, r.box.State))), nil
	case r.kind == rowBox:
		return box.Render(styleDim.Render("\n  No threads. N starts one, t opens a shell.")), nil
	}
	return box.Render(""), nil
}

func (m *tui) listView(w, h int) string {
	rows := h - 1 // under the machine line
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	m.offset = max(min(m.offset, len(m.rows)-rows), 0)
	lines := []string{m.machineLine(w)}
	for i := m.offset; i < len(m.rows) && len(lines) < h; i++ {
		line := m.rowView(m.rows[i], w)
		if i == m.cursor {
			st := styleCursor
			if m.typing || !m.focused {
				st = styleBlurred
			}
			line = st.Render(padRight(ansi.Strip(line), w))
		}
		lines = append(lines, line)
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).Render(strings.Join(lines, "\n"))
}

// machineLine is your machine: whether it's reachable, its memory in use, and how
// many boxes are awake.
func (m *tui) machineLine(w int) string {
	name := machineTarget()
	if name == "" {
		name = "this computer"
	}
	var online, used, total, awake int
	for _, wk := range m.workers {
		if wk.Gone {
			continue
		}
		if wk.Online {
			online++
			used += wk.UsedMiB
			total += wk.MemMiB
		}
	}
	for _, b := range m.boxes {
		if b.State == "running" && b.Kind != api.BoxVersion {
			awake++
		}
	}
	left := " " + styleBold.Render(truncate(name, w/2)) + " "
	var right string
	switch {
	case !m.reached:
		right = styleError.Render("● unreachable")
	case online == 0:
		right = styleError.Render("● offline")
	default:
		right = styleRead.Render("●") + styleDim.Render(fmt.Sprintf(" %.1f/%.1f GB · %d awake", float64(used)/1024, float64(total)/1024, awake))
	}
	return spread(left, right, w)
}

func (m *tui) rowView(r tuiRow, w int) string {
	switch r.kind {
	case rowProject:
		left := " " + styleBold.Render(truncate(r.label, w-4)) + " "
		return left + styleRule.Render(strings.Repeat("─", max(w-lipgloss.Width(left), 0)))
	case rowBox:
		state := ""
		switch r.box.State {
		case "running":
		case "asleep":
			state = "asleep"
		default:
			state = r.box.State
		}
		if r.box.PushError != "" {
			state = "push failed"
		}
		right := styleDim.Render(state)
		branch := ""
		if r.box.Branch != "" {
			branch = styleDim.Render(" ↳ " + r.box.Branch)
		}
		name := styleBold.Render(r.label)
		if r.box.State != "running" {
			name = styleDim.Render(r.label)
		}
		return spread(" "+name+truncateStyled(branch, w-lipgloss.Width(r.label)-lipgloss.Width(state)-4), right, w)
	case rowShell:
		return spread("   "+styleDim.Render("❯")+" shell", "", w)
	}
	t := r.thread
	key := r.key()
	dot := styleDim.Render("●")
	switch {
	case r.box.State != "running":
		dot = styleDim.Render("z")
	case t.State == api.ThreadWorking:
		dot = styleWorking.Render(spinner[m.frame%len(spinner)])
	case t.State == api.ThreadExited:
		dot = styleExited.Render("●")
	case t.State == api.ThreadWaiting && t.Unread && !m.read[key]:
		dot = styleUnread.Render("●")
	case t.State == api.ThreadWaiting:
		dot = styleRead.Render("●")
	}
	right := styleDim.Render(t.Agent + " " + shortAge(t.Created))
	left := fmt.Sprintf("   %s %d ", dot, t.N)
	text := truncate(t.Title, w-lipgloss.Width(left)-lipgloss.Width(right)-1)
	if key == m.shown.Load().(string) {
		text = styleBold.Render(text)
	}
	return spread(left+text, right, w)
}

func (m *tui) footer() string {
	switch {
	case m.inputFor != "":
		label := "New task in " + m.projectName(m.here) + ": "
		if m.inputFor != "new" {
			label = "New thread in " + m.inputFor + ": "
		}
		return " " + styleBold.Render(label) + m.input.View() + styleDim.Render("  enter starts · esc cancels")
	case m.status != "":
		st := styleDim
		if m.confirm != "" {
			st = styleBold
		}
		return " " + st.Render(truncate(m.status, m.w-2))
	case m.typing:
		return styleTyping.Render(" typing ") + styleDim.Render(" ctrl+] gives the keys back")
	}
	keys := "n new · N thread · enter type · t shell · s sleep/wake · d rm · \\ move · z zoom · q quit"
	return styleDim.Render(" " + truncate(keys, m.w-2))
}

// spread puts right at the right edge of a line of width w.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	return left + strings.Repeat(" ", max(gap, 1)) + right
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

func truncateStyled(s string, n int) string {
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// projectDir is where a project is checked out on this machine: where hutch last
// ran in it, else where its boxes came from, if that still exists.
func projectDir(project string, boxes []api.Box) string {
	if d := projectPaths()[project]; d != "" {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	for _, b := range boxes {
		if b.Project == project && b.Path != "" {
			if _, err := os.Stat(b.Path); err == nil {
				return b.Path
			}
		}
	}
	return ""
}

func projectPathsFile() string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "hutch", "projects.json")
}

func projectPaths() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(projectPathsFile()); err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

// rememberProject records where a repository is checked out, for new boxes from
// the TUI.
func rememberProject(id, root string) {
	m := projectPaths()
	if m[id] == root {
		return
	}
	m[id] = root
	data, _ := json.Marshal(m)
	os.MkdirAll(filepath.Dir(projectPathsFile()), 0o700)
	os.WriteFile(projectPathsFile(), data, 0o600)
}

// selfPath is this binary as run, not resolved: package managers link it to
// versioned paths.
func selfPath() (string, error) {
	exe, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func shellJoin(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}
