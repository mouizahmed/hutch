package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The TUI is one program: project tabs on top, the open project's boxes and their
// terminals (threads, in the code) on the left, the selected one on the right, drawn through an in-process emulator. Anything
// it doesn't do itself runs as the CLI command, in the terminal, and comes back.

type rowKind int

const (
	rowBox      rowKind = iota
	rowTask             // a hutch command the TUI runs, until it ends well
	rowTemplate         // a project's template, while it has no template box
	rowThread
)

type tuiRow struct {
	kind    rowKind
	label   string
	box     api.Box
	thread  api.Thread
	task    *task
	project string // rowTemplate's
}

// A task is a hutch command the TUI runs in the background: its output, and the
// step it's on.
type task struct {
	id, title string
	open      string // what to select when it ends well
	lines     []string
	step      string
	err       string
	ended     bool
	started   time.Time
	// report: lines worth keeping in the footer when it ends well; after: what to
	// fetch then.
	report []string
	after  tea.Cmd
}

// key identifies a row across refreshes.
func (r tuiRow) key() string {
	switch r.kind {
	case rowTask:
		return "task " + r.task.id
	case rowTemplate:
		return "template " + r.project
	case rowThread:
		return paneKey(r.box.ID, r.thread.N)
	}
	return r.box.ID
}

type tui struct {
	ctx      context.Context
	sess     *yamux.Session
	prog     *tea.Program
	exe, cwd string
	here     string // the launch folder's project

	listWidth, kept int
	reconnect       time.Duration

	boxes   []api.Box
	workers []api.Worker
	reached bool // the event stream is up
	synced  bool // it has sent the current state
	next    struct {
		boxes   []api.Box
		workers []api.Worker
	} // the current state as it arrives, before synced
	projects map[string]string // id → slug
	rows     []tuiRow
	cursor   int
	offset   int

	panes map[string]*pane
	order []string // pane keys, most recently shown last
	shown atomic.Value
	dirty atomic.Bool

	typing, zoom, below, focused bool
	w, h                         int
	frame                        int

	form        *taskForm // the n and N popup, when open
	confirmText string    // what the confirm popup asks, and its keys
	confirmHint string
	confirmDir  string                    // the checkout a build or snapshot confirm uses
	templates   map[string][]api.Template // each project's versions, newest first
	tasks       []*task
	taskN       int
	agent       string // the agent new tasks and threads start; Tab picks another
	adapters    agents.Set
	models      map[string]string // each agent's chosen model; empty is its default
	confirm     string            // the row key d asks about
	open        string            // box/n to select once it appears
	status      string
	statusAt    time.Time

	read map[string]bool // threads you've looked at since they last worked

	active  string   // the open tab's project, as last chosen here
	closed  []string // tabs closed here
	tabHits []tabHit
	picker  *picker // the project picker, when open
}

type (
	frameMsg    struct{}
	eventMsg    api.Event
	lostMsg     struct{ err error }
	retryMsg    struct{}
	sessMsg     struct{ sess *yamux.Session }
	projectsMsg []api.Project
	agentsMsg   agents.Set
	taskLineMsg struct{ id, line string }
	taskDoneMsg struct {
		id  string
		err error
	}
	paneMsg struct{}
	clipMsg string
	doneMsg struct{ status, open string }
)

var (
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleRule    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	colorCursor  = lipgloss.Color("239")
	colorBlurred = lipgloss.Color("236")
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
		agent: cfg.String("default_agent"), adapters: agents.Builtin(), models: map[string]string{},
		listWidth: int(cfg.Int("tui.list_width")), kept: int(cfg.Int("tui.kept_panes")), reconnect: cfg.Duration("tui.reconnect"),
		projects: map[string]string{}, templates: map[string][]api.Template{}, panes: map[string]*pane{},
		read: map[string]bool{}}
	if repo, _ := config.FindRepo(cwd); repo != nil {
		m.here = repo.ID
	}
	st := loadTabState()
	m.active, m.closed = st.Active, st.Closed
	m.shown.Store("")
	m.prog = tea.NewProgram(m, tea.WithContext(ctx))
	_, err = m.prog.Run()
	for _, p := range m.panes {
		p.close()
	}
	m.sess.Close()
	return err
}

func (m *tui) Init() tea.Cmd {
	ctx, sess := m.ctx, m.sess
	agentList := func() tea.Msg {
		set, err := serverAdapters(ctx, sess)
		if err != nil {
			return nil
		}
		return agentsMsg(set)
	}
	return tea.Batch(m.watch(), m.fetchProjects(), agentList, frame())
}

func frame() tea.Cmd { return tea.Tick(time.Second/8, func(time.Time) tea.Msg { return frameMsg{} }) }

// watch follows the machine's events: the current state, then each change.
func (m *tui) watch() tea.Cmd {
	ctx, sess, prog := m.ctx, m.sess, m.prog
	m.synced = false
	m.next.boxes, m.next.workers = nil, nil
	go func() {
		err := watchEvents(ctx, sess, func(e api.Event) error {
			prog.Send(eventMsg(e))
			return nil
		})
		if err == nil {
			err = fmt.Errorf("the machine closed the connection")
		}
		prog.Send(lostMsg{err})
	}()
	return nil
}

func (m *tui) fetchProjects() tea.Cmd {
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		var ps []api.Project
		if rpc.Call(ctx, sess, api.ProjectList, struct{}{}, &ps) != nil {
			return nil
		}
		return projectsMsg(ps)
	}
}

// layout is where the pane goes: beside the list, below it, or the whole screen
// but the footer.
func (m *tui) layout() (x, y, w, h int) {
	switch {
	case m.zoom:
		return 0, 1, m.w, max(m.h-2, 1)
	case m.below:
		lh := min(len(m.rows)+2, m.h/3)
		return 0, lh + 2, m.w, max(m.h-lh-3, 1)
	}
	lw := min(m.listWidth, m.w/2)
	return lw + 1, 1, max(m.w-lw-1, 1), max(m.h-2, 1)
}

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.resize()
		m.sizeForm()
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
	case paneMsg:
		m.dirty.Store(false)
	case clipMsg:
		return m, tea.SetClipboard(string(msg))
	case agentsMsg:
		m.adapters = agents.Set(msg)
	case projectsMsg:
		var cmds []tea.Cmd
		m.projects = map[string]string{}
		for _, p := range msg {
			m.projects[p.ID] = p.Slug
			cmds = append(cmds, m.fetchTemplates(p.ID))
		}
		for id := range m.templates {
			if m.projects[id] == "" {
				delete(m.templates, id)
			}
		}
		m.build()
		return m, tea.Batch(cmds...)
	case addedMsg:
		return m, m.added(msg)
	case templateWorkMsg:
		return m, m.reviewTemplate(msg)
	case templatesMsg:
		m.templates[msg.project] = msg.versions
		m.build()
	case eventMsg:
		return m, m.event(api.Event(msg))
	case lostMsg:
		if m.reached || m.synced {
			m.setStatus("lost the machine (" + msg.err.Error() + "); trying again")
		}
		m.reached, m.synced = false, false
		return m, tea.Tick(m.reconnect, func(time.Time) tea.Msg { return retryMsg{} })
	case retryMsg:
		if !m.sess.IsClosed() {
			return m, m.watch()
		}
		return m, func() tea.Msg {
			s, err := connect()
			if err != nil {
				return lostMsg{err}
			}
			return sessMsg{s}
		}
	case sessMsg:
		m.sess = msg.sess
		for k := range m.panes {
			m.drop(k)
		}
		return m, tea.Batch(m.watch(), m.fetchProjects())
	case taskLineMsg:
		if t := m.task(msg.id); t != nil {
			t.lines = append(t.lines, msg.line)
			if len(t.lines) > 500 {
				t.lines = t.lines[len(t.lines)-500:]
			}
			if step, ok := strings.CutPrefix(strings.TrimSpace(msg.line), "==> "); ok {
				t.step = step
			}
		}
	case taskDoneMsg:
		if t := m.task(msg.id); t != nil {
			t.ended = true
			var after tea.Cmd
			if msg.err == nil {
				m.tasks = slices.DeleteFunc(m.tasks, func(x *task) bool { return x == t })
				if t.open != "" {
					m.open = t.open
				}
				// What new and thread report starting: "<box>/<n> [<agent>] started …".
				for _, l := range t.lines {
					if f := strings.Fields(l); len(f) > 1 && strings.Contains(f[0], "/") && slices.Contains(f[1:min(len(f), 3)], "started") {
						m.open = f[0]
					}
				}
				var kept []string
				for _, l := range t.lines {
					for _, r := range t.report {
						if strings.HasPrefix(strings.TrimSpace(l), r) {
							kept = append(kept, strings.TrimSpace(l))
						}
					}
				}
				if len(kept) > 0 {
					m.setStatus(t.title + ": " + strings.Join(kept, " · "))
				}
				after = t.after
			} else {
				t.err = lastLine(t.lines)
				if t.err == "" {
					t.err = msg.err.Error()
				}
				m.setStatus(t.title + " failed: " + t.err)
			}
			m.build()
			m.openPending()
			return m, after
		}
	case doneMsg:
		if msg.status != "" {
			m.setStatus(msg.status)
		}
		if msg.open != "" {
			m.open = msg.open
			m.openPending()
		}
	case tea.PasteMsg:
		if m.form != nil {
			return m, m.formPaste(msg)
		}
		if p := m.shownPane(); m.typing && p != nil {
			p.withEmu(func(e *vt.Emulator) { e.Paste(msg.Content) })
		}
	case tea.MouseMsg:
		if m.form != nil || m.confirm != "" || m.picker != nil {
			return m, nil // a popup has the mouse too
		}
		return m, m.mouse(msg)
	case tea.KeyPressMsg:
		switch {
		case m.picker != nil:
			return m, m.pickerKey(msg)
		case m.form != nil:
			return m, m.formKey(msg)
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

// event applies one change. Until the stream says it's synced, the current state
// builds up aside, so boxes removed while disconnected disappear.
func (m *tui) event(e api.Event) tea.Cmd {
	if e.Type == api.EventAlert {
		return m.ring(*e.Thread)
	}
	var cmds []tea.Cmd
	switch {
	case !m.synced && e.Type != api.EventSynced:
		applyEvent(&m.next.boxes, &m.next.workers, e)
		return nil
	case !m.synced:
		if !m.reached {
			m.rows = nil // the first list starts at its top, not on the template row drawn while it loaded
		}
		m.boxes, m.workers = m.next.boxes, m.next.workers
		m.synced, m.reached = true, true
		m.changes(m.boxes)
		cmds = append(cmds, m.fetchProjects())
	default:
		applyEvent(&m.boxes, &m.workers, e)
		m.changes(m.boxes)
		if e.Box != nil && e.Box.Project != "" && m.projects[e.Box.Project] == "" {
			cmds = append(cmds, m.fetchProjects())
		}
	}
	m.build()
	m.openPending()
	return tea.Batch(cmds...)
}

func applyEvent(boxes *[]api.Box, workers *[]api.Worker, e api.Event) {
	findBox := func(id string) int {
		return slices.IndexFunc(*boxes, func(b api.Box) bool { return b.ID == id })
	}
	switch e.Type {
	case api.EventBox:
		b := *e.Box
		if i := findBox(b.ID); i >= 0 {
			b.Threads = (*boxes)[i].Threads
			(*boxes)[i] = b
		} else {
			*boxes = append(*boxes, b)
		}
	case api.EventBoxRemoved:
		if i := findBox(e.ID); i >= 0 {
			*boxes = slices.Delete(*boxes, i, i+1)
		}
	case api.EventThread:
		t := *e.Thread
		if i := findBox(t.Box); i >= 0 {
			ts := (*boxes)[i].Threads
			if j := slices.IndexFunc(ts, func(x api.Thread) bool { return x.N == t.N }); j >= 0 {
				ts[j] = t
			} else {
				ts = append(ts, t)
				slices.SortFunc(ts, func(a, b api.Thread) int { return a.N - b.N })
			}
			(*boxes)[i].Threads = ts
		}
	case api.EventThreadRemoved:
		if i := findBox(e.ID); i >= 0 {
			(*boxes)[i].Threads = slices.DeleteFunc((*boxes)[i].Threads, func(t api.Thread) bool { return t.N == e.N })
		}
	case api.EventWorker:
		w := *e.Worker
		if i := slices.IndexFunc(*workers, func(x api.Worker) bool { return x.ID == w.ID }); i >= 0 {
			(*workers)[i] = w
		} else {
			*workers = append(*workers, w)
		}
	case api.EventWorkerRemoved:
		*workers = slices.DeleteFunc(*workers, func(x api.Worker) bool { return x.ID == e.ID })
	}
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
	case "N", "b", "S", "i":
		if p := templateOf(r); p != "" {
			return m.templateKey(p, msg.String())
		}
	case "[":
		m.stepTab(-1)
		return nil
	case "]":
		m.stepTab(1)
		return nil
	case "p":
		m.openPicker()
		return nil
	case "x":
		m.closeTab(m.project())
		return nil
	}
	switch msg.String() {
	case "n":
		return m.openForm("")
	case "N":
		return m.openForm(r.box.ID)
	case "s":
		if r.box.ID != "" {
			return m.sleepWake(r.box)
		}
	case "d":
		if r.kind == rowTask {
			if r.task.ended {
				m.tasks = slices.DeleteFunc(m.tasks, func(x *task) bool { return x == r.task })
				m.build()
				m.sync()
			} else {
				m.setStatus(r.task.title + " is still running")
			}
			return nil
		}
		if r.box.ID != "" {
			m.confirm = r.key()
			what := "box " + r.box.ID + " and its terminals"
			switch r.kind {
			case rowThread:
				what = "terminal " + r.key() + " (" + r.thread.Title + ")"
			}
			m.confirmText = "Remove " + what + "?"
			m.confirmHint = "y removes · any other key keeps it"
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
	if t, ok := shownThread(r); ok && t.State == api.ThreadExited && t.Agent != "" {
		ctx, sess, target := m.ctx, m.sess, api.ThreadTarget{Box: r.box.ID, N: t.N}
		m.setStatus("starting " + t.Agent + "'s picker in " + paneKey(r.box.ID, t.N) + "…")
		return func() tea.Msg {
			if err := rpc.Call(ctx, sess, api.ThreadResume, target, nil); err != nil {
				return doneMsg{status: err.Error()}
			}
			return doneMsg{status: r.key() + ": pick a conversation (Enter types into it)"}
		}
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

// newProject is where a new box goes: the open tab's project, and its checkout
// here.
func (m *tui) newProject() (project, dir string) {
	if p := m.project(); p != otherTab && p != "" {
		return p, m.checkout(p)
	}
	return "", ""
}

// nextAgent is the agent after (or before) the current one.
func (m *tui) nextAgent(forward bool) string {
	names := m.runChoices()
	i := slices.Index(names, m.agent)
	if len(names) == 0 {
		return m.agent
	}
	if forward {
		return names[(i+1)%len(names)]
	}
	return names[(max(i, 0)+len(names)-1)%len(names)]
}

func (m *tui) confirmKey(msg tea.KeyPressMsg) tea.Cmd {
	key := m.confirm
	m.confirm = ""
	if msg.String() != "y" {
		return nil
	}
	for _, verb := range []string{"build", "snapshot"} {
		if project, ok := strings.CutPrefix(key, verb+" "); ok {
			return m.templateTask(project, m.confirmDir, verb)
		}
	}
	for _, r := range m.rows {
		if r.key() != key {
			continue
		}
		switch r.kind {
		case rowThread:
			m.drop(key)
			return m.run(m.cwd, "remove "+key, "", "terminal", "rm", key)
		default:
			for k, p := range m.panes {
				if p.box == r.box.ID {
					m.drop(k)
				}
			}
			return m.run(m.cwd, "remove "+r.box.ID, "", "rm", r.box.ID)
		}
	}
	return nil
}

// run runs a hutch command in the background, in dir, as a task row at the top of
// the list whose pane shows its output; it goes when the command ends well, and
// open is selected. A failed task stays, with its output, until d dismisses it.
func (m *tui) run(dir, title, open string, args ...string) tea.Cmd {
	return m.start(dir, title, open, exec.Command(m.exe, args...))
}

// start runs c as a task (see run).
func (m *tui) start(dir, title, open string, c *exec.Cmd) tea.Cmd {
	m.taskN++
	t := &task{id: strconv.Itoa(m.taskN), title: title, open: open, step: "starting", started: time.Now()}
	m.tasks = append(m.tasks, t)
	m.build()
	m.selectKey("task " + t.id)
	m.sync()
	prog := m.prog
	c.Dir = dir
	c.Env = append(os.Environ(), "NO_COLOR=1")
	go func() {
		r, w, err := os.Pipe()
		if err != nil {
			prog.Send(taskDoneMsg{t.id, err})
			return
		}
		c.Stdout, c.Stderr = w, w
		err = c.Start()
		w.Close()
		if err != nil {
			r.Close()
			prog.Send(taskDoneMsg{t.id, err})
			return
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			prog.Send(taskLineMsg{t.id, ansi.Strip(sc.Text())})
		}
		r.Close()
		prog.Send(taskDoneMsg{t.id, c.Wait()})
	}()
	return nil
}

func (m *tui) task(id string) *task {
	for _, t := range m.tasks {
		if t.id == id {
			return t
		}
	}
	return nil
}

// lastLine is a command's last line of output, its error when it fails.
func lastLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(strings.TrimPrefix(lines[i], "hutch: ")); l != "" {
			return l
		}
	}
	return ""
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
	if _, click := msg.(tea.MouseClickMsg); click && mo.Y == 0 {
		return m.clickTab(mo.X)
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
		if i := m.offset + mo.Y - 2; mo.Y > 1 && i < len(m.rows) {
			m.cursor = i
			m.sync()
		}
	}
	return nil
}

// changes forgets that you read threads that are working again.
func (m *tui) changes(boxes []api.Box) {
	for _, b := range boxes {
		for _, t := range b.Threads {
			if t.State == api.ThreadWorking {
				delete(m.read, paneKey(b.ID, t.N))
			}
		}
	}
}

// ring is an agent wanting you, unless you're looking at it: the bell, and a
// desktop notification (OSC 9).
func (m *tui) ring(t api.Thread) tea.Cmd {
	key := paneKey(t.Box, t.N)
	if i := slices.IndexFunc(m.boxes, func(b api.Box) bool { return b.ID == t.Box }); i >= 0 {
		project := m.boxes[i].Project
		if project == "" {
			project = otherTab
		}
		m.reopen(project)
	}
	if m.looking(key) {
		return nil
	}
	verb := "wants you"
	if t.State == api.ThreadExited {
		verb = "exited"
	}
	return tea.Raw(fmt.Sprintf("\a\x1b]9;hutch: %s %s: %s\a", key, verb, strings.ReplaceAll(t.Title, "\a", "")))
}

func (m *tui) looking(key string) bool { return m.focused && m.shown.Load().(string) == key }

// build lays out the rows: running tasks, then the open tab's boxes, most
// recently started first, with their threads, and its template last.
func (m *tui) build() {
	selected := m.selected().key()
	project := m.project()
	var boxes []api.Box
	for _, b := range m.boxes {
		if b.Kind != api.BoxVersion && m.inTab(b) {
			boxes = append(boxes, b)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool {
		if (boxes[i].Kind == api.BoxTemplate) != (boxes[j].Kind == api.BoxTemplate) {
			return boxes[j].Kind == api.BoxTemplate
		}
		return boxActive(boxes[i]).After(boxActive(boxes[j]))
	})
	m.rows = nil
	for _, t := range m.tasks {
		m.rows = append(m.rows, tuiRow{kind: rowTask, label: t.title, task: t})
	}
	templateBox := false
	for _, b := range boxes {
		label := b.ID
		if b.Kind == api.BoxTemplate {
			label, templateBox = m.templateLabel(b.Project), true
		}
		m.rows = append(m.rows, tuiRow{kind: rowBox, box: b, label: label})
		for _, t := range b.Threads {
			m.rows = append(m.rows, tuiRow{kind: rowThread, box: b, thread: t, label: t.Title})
		}
	}
	if project != "" && project != otherTab && !templateBox {
		m.rows = append(m.rows, tuiRow{kind: rowTemplate, project: project, label: m.templateLabel(project)})
	}
	if !m.selectKey(selected) {
		m.cursor = max(min(m.cursor, len(m.rows)-1), 0)
	}
	m.resize() // below the list, the pane follows the row count
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
		if r.key() == key {
			m.cursor = i
			return true
		}
	}
	return false
}

func (m *tui) move(d int) {
	if i := m.cursor + d; i >= 0 && i < len(m.rows) {
		m.cursor = i
		m.typing = false
		m.sync()
	}
}

// target is the terminal a row shows: its thread, its shell, or for a box its
// latest thread. A box that isn't running shows none: nothing wakes by browsing.
func (m *tui) target(r tuiRow) (box string, n int, ok bool) {
	t, ok := shownThread(r)
	if r.box.State != "running" || !ok {
		return "", 0, false
	}
	return r.box.ID, t.N, true
}

// shownThread is the thread a row shows: its own, or a box's latest.
func shownThread(r tuiRow) (api.Thread, bool) {
	switch {
	case r.kind == rowThread:
		return r.thread, true
	case r.kind == rowBox && len(r.box.Threads) > 0:
		return r.box.Threads[len(r.box.Threads)-1], true
	}
	return api.Thread{}, false
}

// openPending selects what a finished command made, once it's listed.
func (m *tui) openPending() {
	if m.open != "" && m.selectKey(m.open) {
		m.open = ""
	}
	m.sync()
}

// sync shows the selected row's terminal, connecting it if it isn't kept, and
// drops terminals of boxes that stopped and those past tui.kept_panes.
func (m *tui) sync() {
	live := map[string]bool{}
	for _, b := range m.boxes {
		for _, t := range b.Threads {
			live[paneKey(b.ID, t.N)] = b.State == "running"
		}
	}
	for k := range m.panes {
		if !live[k] {
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
	changed := func() {
		if m.shown.Load().(string) == key && m.dirty.CompareAndSwap(false, true) {
			go m.prog.Send(paneMsg{})
		}
	}
	go p.run(m.ctx, m.sess, changed, func(s string) { go m.prog.Send(clipMsg(s)) })
}

func (m *tui) drop(key string) {
	if p := m.panes[key]; p != nil {
		p.close()
		delete(m.panes, key)
	}
	m.order = slices.DeleteFunc(m.order, func(k string) bool { return k == key })
}

// markHidden tells each kept thread terminal whether you see it: off screen, the
// box may sleep; on screen while the terminal isn't focused, you aren't looking.
func (m *tui) markHidden() {
	for key, p := range m.panes {
		p.setHidden(m.ctx, m.sess, m.hiddenMark(key))
	}
}

func (m *tui) hiddenMark(key string) string {
	switch {
	case m.shown.Load().(string) != key:
		return guestproto.HiddenOff
	case !m.focused:
		return guestproto.HiddenAway
	}
	return ""
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
		lh := py - 2
		body = lipgloss.JoinVertical(lipgloss.Left, m.listView(m.w, lh), styleRule.Render(strings.Repeat("─", m.w)), screen)
	default:
		sep := styleRule.Render(strings.TrimSuffix(strings.Repeat("│\n", ph), "\n"))
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listView(px-1, ph), sep, screen)
	}
	screen = lipgloss.JoinVertical(lipgloss.Left, m.tabBar(m.w), body, m.footer())
	switch {
	case m.picker != nil:
		screen = m.overlay(screen, m.pickerView())
	case m.form != nil:
		screen = m.overlay(screen, m.formView())
	case m.confirm != "":
		screen = m.overlay(screen, m.confirmView())
	}
	v := tea.NewView(screen)
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
	exited, _ := shownThread(r)
	switch {
	case len(m.rows) == 0:
		if m.project() == "" {
			return box.Render(styleDim.Render("\n  No projects yet. p adds a checkout.")), nil
		}
		return box.Render(styleDim.Render("\n  No boxes here yet. n starts a task in a new box.")), nil
	case exited.Agent != "" && exited.State == api.ThreadExited && r.box.State == "running":
		// Its last screen stays, under what Enter does now.
		hint := styleDim.Render(exited.Agent+" exited. ") + styleBold.Render("Enter") + styleDim.Render(" opens its conversation picker here; d removes the terminal.")
		if p == nil || p.ended.Load() || h < 2 {
			return box.Render("\n  " + hint), nil
		}
		s, _ := p.render()
		lines := strings.Split(s, "\n")
		return box.Render(strings.Join(lines[:min(len(lines), h-1)], "\n") + "\n" + truncate(hint, w)), nil
	case r.kind == rowTemplate:
		lines := []string{"", "  The " + m.projectName(r.project) + " template: new boxes start from its newest version.", ""}
		for i, v := range m.templates[r.project] {
			if i == 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("  v%d  %.12s  %s", v.Version, v.Commit, ago(v.Created)))
		}
		lines = append(lines, "", "  N  a terminal on the template: an agent (\"add Playwright browsers\"), a shell, a command",
			"  b  build: brings the template box's work here, commits and pushes .hutch/, builds that",
			"  S  snapshot the build as the next version")
		return box.Render(styleDim.Render(strings.Join(lines, "\n"))), nil
	case r.kind == rowTask:
		t := r.task
		head := styleBold.Render(t.title) + styleDim.Render(fmt.Sprintf(" · %s · %s", t.step, time.Since(t.started).Round(time.Second)))
		if t.ended {
			head = styleError.Render(t.title+" failed") + styleDim.Render(" · d dismisses")
		}
		var lines []string
		for _, l := range t.lines[max(len(t.lines)-max(h-2, 0), 0):] {
			lines = append(lines, truncate(l, w-1))
		}
		return box.Render(head + "\n\n" + strings.Join(lines, "\n")), nil
	case p != nil && p.ended.Load():
		why, _ := p.err.Load().(string)
		return box.Render(styleDim.Render(fmt.Sprintf("\n  %s: %s. Enter reconnects.", m.shown.Load(), why))), nil
	case p != nil:
		s, cur := p.render()
		return box.Render(s), cur
	case r.box.ID != "" && r.box.State != "running":
		return box.Render(styleDim.Render(fmt.Sprintf("\n  %s is %s. Enter wakes it.", r.box.ID, r.box.State))), nil
	case r.kind == rowBox:
		return box.Render(styleDim.Render("\n  No terminals. N starts one: an agent, a shell, or a command.")), nil
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
			bg := colorCursor
			if m.typing || !m.focused {
				bg = colorBlurred
			}
			line = highlight(line, w, bg)
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
	case rowBox:
		state := ""
		switch r.box.State {
		case "running":
		case "asleep":
			state = "asleep"
		default:
			state = r.box.State
		}
		// Work only in the box: commits on no remote, and uncommitted changes.
		var only []string
		if r.box.Unpushed > 0 {
			only = append(only, fmt.Sprintf("%d unpushed", r.box.Unpushed))
		}
		if r.box.Changes > 0 {
			only = append(only, "edited")
		}
		if state != "" {
			only = append([]string{state}, only...)
		}
		state = strings.Join(only, " · ")
		right := styleDim.Render(state)
		branch := ""
		// A box's own hutch/<box> branch goes without saying.
		if r.box.Branch != "" && r.box.Branch != "hutch/"+r.box.ID {
			branch += styleWorking.Render(" ↳ " + r.box.Branch)
		}
		name := styleBold.Render(r.label)
		if r.box.State != "running" {
			name = styleDim.Render(r.label)
		}
		return spread(" "+name+truncate(branch, w-lipgloss.Width(r.label)-lipgloss.Width(state)-4), right, w)
	case rowTemplate:
		right := "none yet: b builds it"
		switch {
		case m.projects[r.project] == "" || !m.hasRecipe(r.project):
			right = "not set up: i"
		case len(m.templates[r.project]) > 0:
			right = shortAge(m.templates[r.project][0].Created)
		}
		return spread(" "+styleDim.Render(r.label), styleDim.Render(right), w)
	case rowTask:
		t := r.task
		dot, step := styleWorking.Render(spinner[m.frame%len(spinner)]), t.step
		if t.ended {
			dot, step = styleError.Render("!"), "failed"
		}
		right := styleDim.Render(truncate(step, w/2))
		return spread(" "+dot+" "+truncate(t.title, w-lipgloss.Width(right)-5), right, w)
	}
	t := r.thread
	key := r.key()
	dot := styleDim.Render("●")
	switch {
	case r.box.State != "running" && t.Unread && !m.read[key]:
		dot = styleUnread.Render("●") // asleep, with something you haven't seen
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
	case m.status != "":
		return " " + styleDim.Render(truncate(m.status, m.w-2))
	case m.typing:
		return styleTyping.Render(" typing ") + styleDim.Render(" ctrl+] gives the keys back")
	}
	keys := "n new box · N new terminal · enter type · [ ] p tabs · s sleep/wake · d rm · \\ move · z zoom · q quit"
	if p := templateOf(m.selected()); p != "" {
		keys = "N new terminal · b build · S snapshot · n new box · [ ] p tabs · q quit"
		if m.projects[p] == "" || !m.hasRecipe(p) {
			keys = "i set up (hutch init) · [ ] p tabs · q quit"
		}
	}
	return styleDim.Render(" " + truncate(keys, m.w-2))
}

// highlight puts bg behind a styled line of width w, keeping its colors.
func highlight(line string, w int, bg color.Color) string {
	buf := uv.NewScreenBuffer(w, 1)
	uv.NewStyledString(line).Draw(buf, buf.Bounds())
	for x := 0; x < w; x++ {
		c := buf.CellAt(x, 0)
		if c == nil {
			continue
		}
		cell := *c
		if cell.Content == "" && cell.Width == 0 {
			cell = uv.EmptyCell
		}
		cell.Style.Bg = bg
		buf.SetCell(x, 0, &cell)
	}
	return buf.Render()
}

// spread puts right at the right edge of a line of width w.
func spread(left, right string, w int) string {
	if room := w - lipgloss.Width(right) - 1; lipgloss.Width(left) > room {
		left = truncate(left, room)
	}
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

// truncate fits s in n terminal cells, for plain and styled text alike.
func truncate(s string, n int) string {
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
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
