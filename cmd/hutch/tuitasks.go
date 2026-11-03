package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Tasks are the hutch commands the TUI runs in the background, each a row at the
// top of the list whose pane shows its output. One that ends well leaves the
// list; one that fails or is canceled stays until d dismisses it.

type taskState string

const (
	taskRunning  taskState = "running"
	taskFailed   taskState = "failed"
	taskCanceled taskState = "canceled" // stopped here; what it did on the machine is unknown
)

type task struct {
	ID      string
	Title   string
	Lines   []string
	Dropped int // earlier lines no longer kept
	Step    string
	Err     string
	State   taskState
	Started time.Time

	open      string // what to select when it ends well
	report    []string
	after     tea.Cmd
	cmd       *exec.Cmd
	done      chan struct{} // closed when cmd has exited
	canceling bool
}

func (t *task) running() bool { return t.State == taskRunning }

// run runs a hutch command in the background, in dir, as a task row; when it
// ends well, open is selected.
func (m *tui) run(dir, title, open string, args ...string) tea.Cmd {
	m.taskN++
	t := &task{ID: strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(m.taskN), Title: title,
		open: open, Step: "starting", State: taskRunning, Started: time.Now()}
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
	t.cmd = nil
	var after tea.Cmd
	switch {
	case t.canceling:
		t.State, t.Err = taskCanceled, "canceled here: the machine may have done some or all of it; check before running it again"
		m.setStatus(t.Title + ": canceled")
	case msg.err == nil:
		m.tasks = slices.DeleteFunc(m.tasks, func(x *task) bool { return x == t })
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
			m.setStatus(t.Title + ": done")
		}
		after = t.after
	default:
		t.State = taskFailed
		t.Err = lastLine(t.Lines)
		if t.Err == "" {
			t.Err = msg.err.Error()
		}
		m.setStatus(t.Title + " failed: " + t.Err)
	}
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
// tui.cancel_grace before stopping them for good.
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
		t.State = taskCanceled
	}
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

func taskMark(t *task) string {
	if t.State == taskFailed {
		return styleError.Render("!")
	}
	return styleExited.Render("?")
}
