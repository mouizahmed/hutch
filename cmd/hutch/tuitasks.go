package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Tasks are the hutch commands the TUI runs in the background. Each keeps its
// command (to look at, and to retry deliberately), its output, and how it
// ended; finished ones are kept, bounded, in this computer's cache.

type taskState string

const (
	taskRunning  taskState = "running"
	taskOK       taskState = "done"
	taskFailed   taskState = "failed"
	taskCanceled taskState = "canceled" // stopped here; what it did on the machine is unknown
	taskUnknown  taskState = "unknown"  // Hutch quit while it ran
)

// A task is a hutch command the TUI runs in the background: its output, and the
// step it's on.
type task struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Dir     string    `json:"dir"`
	Args    []string  `json:"args"`
	Lines   []string  `json:"lines"`
	Dropped int       `json:"dropped,omitempty"` // earlier lines no longer kept
	Step    string    `json:"step,omitempty"`
	Err     string    `json:"err,omitempty"`
	State   taskState `json:"state"`
	Code    int       `json:"code,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`

	open      string // what to select when it ends well
	report    []string
	after     tea.Cmd
	cmd       *exec.Cmd
	done      chan struct{} // closed when cmd has exited
	canceling bool
	dismissed bool // off the list; still in Tasks
}

func (t *task) running() bool { return t.State == taskRunning }

// listed reports whether the task has a row: running, or ended badly and not
// dismissed.
func (t *task) listed() bool {
	return t.running() || t.State != taskOK && !t.dismissed
}

// run runs a hutch command in the background, in dir, as a task row at the top of
// the list whose pane shows its output; it leaves the list when the command ends
// well, and open is selected. One that ends badly stays until d dismisses it.
func (m *tui) run(dir, title, open string, args ...string) tea.Cmd {
	m.taskN++
	t := &task{ID: strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(m.taskN), Title: title, Dir: dir,
		Args: args, open: open, Step: "starting", State: taskRunning, Started: time.Now()}
	m.tasks = append(m.tasks, t)
	m.build()
	m.selectKey("task " + t.ID)
	m.sync()
	c := exec.Command(m.exe, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "NO_COLOR=1")
	ownGroup(c)
	// Started here, so a cancel always has the process to stop.
	r, w, err := os.Pipe()
	if err == nil {
		c.Stdout, c.Stderr = w, w
		err = c.Start()
		w.Close()
		if err != nil {
			r.Close()
		}
	}
	if err != nil {
		return func() tea.Msg { return taskDoneMsg{t.ID, err} }
	}
	t.cmd, t.done = c, make(chan struct{})
	prog := m.prog
	go func() {
		readLines(r, func(line string) { prog.Send(taskLineMsg{t.ID, ansi.Strip(line)}) })
		r.Close()
		err := c.Wait()
		close(t.done)
		prog.Send(taskDoneMsg{t.ID, err})
	}()
	return nil
}

// readLines calls line for each line of r; one longer than 64K comes in pieces,
// so no output stops the reading.
func readLines(r io.Reader, line func(string)) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		b, _, err := br.ReadLine()
		if err != nil {
			return
		}
		line(string(b))
	}
}

func (m *tui) task(id string) *task {
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (m *tui) taskLine(msg taskLineMsg) {
	t := m.task(msg.id)
	if t == nil {
		return
	}
	t.Lines = append(t.Lines, msg.line)
	if n := len(t.Lines) - m.taskLines; n > 0 {
		t.Lines, t.Dropped = t.Lines[n:], t.Dropped+n
	}
	if step, ok := strings.CutPrefix(strings.TrimSpace(msg.line), "==> "); ok {
		t.Step = step
	}
}

func (m *tui) taskDone(msg taskDoneMsg) tea.Cmd {
	t := m.task(msg.id)
	if t == nil || !t.running() {
		return nil
	}
	t.Ended, t.cmd = time.Now(), nil
	var after tea.Cmd
	switch {
	case t.canceling:
		t.State, t.Err = taskCanceled, "canceled here: the machine may have done some or all of it; check before retrying"
		m.setStatus(t.Title + ": canceled")
	case msg.err == nil:
		t.State = taskOK
		if t.open != "" {
			m.open = t.open
		}
		// What new and thread report starting: "<box>/<n> [<agent>] started …".
		for _, l := range t.Lines {
			if f := strings.Fields(l); len(f) > 1 && strings.Contains(f[0], "/") && slices.Contains(f[1:min(len(f), 3)], "started") {
				m.open = f[0]
			}
		}
		var kept []string
		for _, l := range t.Lines {
			for _, r := range t.report {
				if strings.HasPrefix(strings.TrimSpace(l), r) {
					kept = append(kept, strings.TrimSpace(l))
				}
			}
		}
		if len(kept) > 0 {
			m.setStatus(t.Title + ": " + strings.Join(kept, " · "))
		} else {
			m.setStatus(t.Title + ": done (t lists tasks)")
		}
		after = t.after
	default:
		t.State = taskFailed
		if exit, ok := msg.err.(*exec.ExitError); ok {
			t.Code = exit.ExitCode()
		}
		t.Err = lastLine(t.Lines)
		if t.Err == "" {
			t.Err = msg.err.Error()
		}
		m.setStatus(t.Title + " failed: " + t.Err)
	}
	m.trimTasks()
	m.saveTasks()
	m.build()
	m.openPending()
	return after
}

// cancelTask stops a running task's command and what it started: gently, then
// for good after tui.cancel_grace, or at once when it's already being canceled.
// The machine may already have done some of it.
func (m *tui) cancelTask(t *task) {
	if !t.running() || t.cmd == nil {
		return
	}
	c := t.cmd
	if t.canceling {
		signalGroup(c, true)
		return
	}
	t.canceling, t.Step = true, "canceling"
	signalGroup(c, false)
	time.AfterFunc(m.cancelGrace, func() { signalGroup(c, true) })
	m.setStatus(t.Title + ": canceling")
}

// stopTasks cancels every running task as Hutch quits, and waits for them: up to
// tui.cancel_grace before stopping them for good. What still runs after that is
// left as unknown.
func (m *tui) stopTasks() {
	var running []*task
	for _, t := range m.tasks {
		if t.running() && t.cmd != nil {
			m.cancelTask(t)
			running = append(running, t)
		}
	}
	deadline := make(chan struct{})
	time.AfterFunc(m.cancelGrace, func() { close(deadline) })
	for _, t := range running {
		select {
		case <-t.done:
		case <-deadline:
			signalGroup(t.cmd, true)
			select {
			case <-t.done:
			case <-time.After(time.Second):
				continue
			}
		}
		t.State, t.Ended = taskCanceled, time.Now()
		t.Err = "canceled when Hutch quit: the machine may have done some or all of it; check before retrying"
	}
}

// retryTask asks before running a task's command again: nothing is replayed on
// its own.
func (m *tui) retryTask(t *task) {
	if t.running() {
		m.setStatus(t.Title + " is still running")
		return
	}
	m.ask("Run again: "+commandLine(t.Args)+"\n  in "+t.Dir+"\n\nIt ran before and ended "+string(t.State)+"; check what it already changed.",
		"y runs it again · any other key keeps it as is", func() tea.Cmd {
			return m.run(t.Dir, t.Title, "", t.Args...)
		})
}

// trimTasks forgets the oldest finished tasks past tui.task_history, and any
// past tui.task_expiry.
func (m *tui) trimTasks() {
	var finished int
	for i := len(m.tasks) - 1; i >= 0; i-- {
		t := m.tasks[i]
		if t.running() {
			continue
		}
		if time.Since(t.Ended) > m.taskExpiry {
			m.tasks = slices.Delete(m.tasks, i, i+1)
			continue
		}
		if finished++; finished > m.taskHistory {
			m.tasks = slices.Delete(m.tasks, i, i+1)
		}
	}
}

func tasksFile() string { return filepath.Join(filepath.Dir(projectPathsFile()), "tasks.json") }

// saveTasks keeps the tasks in this computer's cache (one still running comes
// back as unknown), readable only by
// you. Logins and other interactive flows never run as tasks, so no secret entry
// is in their output.
func (m *tui) saveTasks() {
	data, err := json.Marshal(m.tasks)
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(tasksFile()), 0o700)
	tmp := tasksFile() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, tasksFile())
	}
}

// loadTasks reads the tasks kept from before; one still running when Hutch quit
// ended without its outcome being seen.
func (m *tui) loadTasks() {
	data, err := os.ReadFile(tasksFile())
	if err != nil {
		return
	}
	var ts []*task
	if json.Unmarshal(data, &ts) != nil {
		return
	}
	for _, t := range ts {
		if t.State == taskRunning {
			t.State, t.Err = taskUnknown, "Hutch quit while it ran; its outcome wasn't seen"
		}
		t.dismissed = true // from an earlier run: in Tasks, not the list
	}
	m.tasks = ts
	m.trimTasks()
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

// The Tasks view: every task, newest first; Enter opens one's whole output.
type tasksView struct {
	cursor int
	open   *task // whose output is shown
	scroll int   // lines from the top of the output
	follow bool  // keep the end in view as output arrives
	query  string
}

func (m *tui) openTasks(t *task) {
	m.taskv = &tasksView{}
	if t != nil {
		m.taskv.open, m.taskv.follow = t, true
	}
	m.setView(viewTasks)
}

// newestTasks is the tasks, newest first.
func (m *tui) newestTasks() []*task {
	ts := slices.Clone(m.tasks)
	slices.Reverse(ts)
	return ts
}

func taskMark(t *task) string {
	switch t.State {
	case taskRunning:
		return styleWorking.Render("●")
	case taskOK:
		return styleRead.Render("✓")
	case taskFailed:
		return styleError.Render("!")
	}
	return styleExited.Render("?")
}

func (m *tui) tasksKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.taskv
	ts := m.newestTasks()
	if v.open == nil {
		switch msg.String() {
		case "j", "down":
			v.cursor = min(v.cursor+1, len(ts)-1)
		case "k", "up":
			v.cursor = max(v.cursor-1, 0)
		case "enter":
			if v.cursor < len(ts) {
				v.open, v.scroll, v.follow = ts[v.cursor], 0, ts[v.cursor].running()
			}
		case "C":
			m.ask("Forget every finished task?", "y forgets them · any other key keeps them", func() tea.Cmd {
				m.tasks = slices.DeleteFunc(m.tasks, func(t *task) bool { return !t.running() })
				m.saveTasks()
				m.build()
				return nil
			})
		case "esc", "q", "t":
			m.setView(viewNone)
		}
		return nil
	}
	t := v.open
	_, _, _, h := m.layout()
	page := max(h-4, 1)
	switch msg.String() {
	case "j", "down":
		v.scroll++
		v.follow = false
	case "k", "up":
		v.scroll = max(v.scroll-1, 0)
		v.follow = false
	case "pgdown", "space", "ctrl+d":
		v.scroll += page
		v.follow = false
	case "pgup", "ctrl+u":
		v.scroll = max(v.scroll-page, 0)
		v.follow = false
	case "g", "home":
		v.scroll, v.follow = 0, false
	case "G", "end":
		v.follow = true
	case "/":
		m.input("Search the output", "", v.query, func(q string) tea.Cmd {
			v.query = q
			m.findInTask(v, 0)
			return nil
		})
	case "n":
		m.findInTask(v, v.scroll+1)
	case "y":
		m.setStatus("copied " + t.Title + "'s output")
		return tea.SetClipboard(strings.Join(t.Lines, "\n"))
	case "c":
		if !t.running() {
			m.setStatus(t.Title + " isn't running")
			return nil
		}
		m.ask("Cancel "+t.Title+"?\n\nIt stops here, but the machine may already have done some or all of it.", "y cancels · any other key lets it run", func() tea.Cmd {
			m.cancelTask(t)
			return nil
		})
	case "R":
		m.retryTask(t)
	case "d":
		if t.running() {
			m.setStatus(t.Title + " is still running: c cancels it")
			return nil
		}
		t.dismissed = true
		m.build()
	case "esc", "q":
		v.open = nil
	}
	return nil
}

// findInTask moves to the next line from line from that holds the query.
func (m *tui) findInTask(v *tasksView, from int) {
	if v.query == "" || v.open == nil {
		return
	}
	q := strings.ToLower(v.query)
	lines := v.open.Lines
	for i := range lines {
		j := (from + i) % len(lines)
		if strings.Contains(strings.ToLower(lines[j]), q) {
			v.scroll, v.follow = j, false
			return
		}
	}
	m.setStatus("no line has " + strconv.Quote(v.query))
}

func (m *tui) tasksView(w, h int) string {
	v := m.taskv
	if v.open == nil {
		ts := m.newestTasks()
		lines := []string{styleBold.Render("Tasks") + styleDim.Render(fmt.Sprintf("  %d kept · enter output · C forget finished · esc back", len(ts))), ""}
		if len(ts) == 0 {
			lines = append(lines, styleDim.Render("  No tasks yet: what the TUI runs for you shows here, with its output."))
		}
		v.cursor = max(min(v.cursor, len(ts)-1), 0)
		top := max(v.cursor-(h-4), 0)
		for i := top; i < len(ts) && len(lines) < h; i++ {
			t := ts[i]
			when := shortAge(t.Started)
			right := styleDim.Render(string(t.State) + " " + when)
			line := spread(" "+taskMark(t)+" "+truncate(t.Title, w-20), right, w-1)
			if i == v.cursor {
				line = highlight(line, w-1, colorCursor)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	t := v.open
	head := taskMark(t) + " " + styleBold.Render(truncate(t.Title, w/2))
	info := fmt.Sprintf("  %s · %s", t.State, shortAge(t.Started))
	if t.State == taskFailed && t.Code != 0 {
		info += fmt.Sprintf(" · exit %d", t.Code)
	}
	cmdLine := styleDim.Render(truncate(commandLine(t.Args), w-2))
	rows := max(h-5, 1)
	body := t.Lines
	if v.follow {
		v.scroll = max(len(body)-rows, 0)
	}
	v.scroll = max(min(v.scroll, max(len(body)-rows, 0)), 0)
	var out []string
	if t.Dropped > 0 && v.scroll == 0 {
		out = append(out, styleDim.Render(fmt.Sprintf("… %d earlier lines weren't kept (tui.task_lines)", t.Dropped)))
	}
	for i := v.scroll; i < len(body) && len(out) < rows; i++ {
		l := truncate(body[i], w-1)
		if v.query != "" && strings.Contains(strings.ToLower(body[i]), strings.ToLower(v.query)) {
			l = styleWorking.Render(l)
		}
		out = append(out, l)
	}
	if t.Err != "" && !t.running() {
		out = append(out, styleError.Render(truncate(t.Err, w-1)))
	}
	keys := "j/k pgup/pgdn g/G scroll · / search · n next · y copy · R retry · d dismiss · esc"
	if t.running() {
		keys = "j/k pgup/pgdn g/G scroll · / search · y copy · c cancel · esc"
	}
	return strings.Join(append([]string{head + styleDim.Render(info), cmdLine, ""}, out...), "\n") + "\n" + styleDim.Render(truncate(keys, w-1))
}

// commandLine is a hutch command as you'd type it, quoting what needs it.
func commandLine(args []string) string {
	out := []string{"hutch"}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]#~") {
			a = shellQuote(a)
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}
