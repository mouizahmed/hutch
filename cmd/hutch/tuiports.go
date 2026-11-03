package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The Ports view lists what a box listens on and forwards a port to this
// computer's loopback, as hutch forward does: a forward to a sleeping box
// wakes it on its first connection, and open connections keep it awake. The
// TUI owns its forwards: they stay while you look elsewhere, and close when you
// stop them or quit.

type forward struct {
	box           string
	remote, local int
	ln            net.Listener
	cancel        context.CancelFunc
	started       time.Time
}

func (f *forward) addr() string { return "localhost:" + strconv.Itoa(f.local) }

func forwardKey(box string, remote int) string { return box + ":" + strconv.Itoa(remote) }

type portsView struct {
	box    api.Box
	ports  []guestproto.ListenPort
	read   time.Time // when the listeners were read
	err    string
	cursor int
}

type portsMsg struct {
	box    string
	status guestproto.Status
	err    error
}

func (m *tui) openPorts(b api.Box) tea.Cmd {
	m.portv = &portsView{box: b}
	m.setView(viewPorts)
	return m.readPorts()
}

// readPorts asks a running box what it listens on; a sleeping box isn't woken
// to answer.
func (m *tui) readPorts() tea.Cmd {
	v := m.portv
	if v.box.State != "running" {
		return nil
	}
	ctx, sess, box := m.ctx, m.sess, v.box.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var st guestproto.Status
		err := rpc.Call(ctx, sess, api.BoxStatus, api.IDParams{ID: box}, &st)
		return portsMsg{box: box, status: st, err: err}
	}
}

// rows are the ports listed: what the box listens on, and forwards to ports it
// doesn't (or might not, asleep).
func (v *portsView) rows(fs map[string]*forward) []int {
	var out []int
	for _, p := range v.ports {
		out = append(out, p.Port)
	}
	for _, f := range fs {
		if f.box == v.box.ID && !slices.Contains(out, f.remote) {
			out = append(out, f.remote)
		}
	}
	slices.Sort(out)
	return out
}

func (m *tui) portsKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.portv
	rows := v.rows(m.forwards)
	var port int
	if v.cursor < len(rows) {
		port = rows[v.cursor]
	}
	f := m.forwards[forwardKey(v.box.ID, port)]
	switch msg.String() {
	case "esc", "q":
		m.setView(viewNone)
	case "j", "down":
		v.cursor = min(v.cursor+1, len(rows)-1)
	case "k", "up":
		v.cursor = max(v.cursor-1, 0)
	case "r":
		if v.box.State != "running" {
			m.setStatus(v.box.ID + " is asleep: reading its listeners would wake it. a forwards a port you know.")
			return nil
		}
		return m.readPorts()
	case "a":
		m.input("Forward a port of "+v.box.ID+" (the box wakes on the first connection)", "port, or port:local port", "", func(s string) tea.Cmd {
			remote, local, err := parsePorts(s)
			if err != nil {
				m.setStatus(err.Error())
				return nil
			}
			return m.startForward(v.box.ID, remote, local)
		})
	case "enter":
		if port == 0 {
			return nil
		}
		if f != nil {
			m.stopForward(f)
			m.setStatus("stopped forwarding " + v.box.ID + ":" + strconv.Itoa(port))
			return nil
		}
		return m.startForward(v.box.ID, port, port)
	case "y":
		if f != nil {
			m.setStatus("copied " + f.addr())
			return tea.SetClipboard(f.addr())
		}
	case "O":
		if f == nil {
			m.setStatus("forward it first: enter")
			return nil
		}
		url := "http://" + f.addr()
		m.ask("Open "+url+" in your browser?\n\nIt's "+v.box.ID+":"+strconv.Itoa(f.remote)+"; Hutch doesn't know whether it speaks HTTP.", "y opens it · any other key doesn't", func() tea.Cmd {
			if err := openURL(url); err != nil {
				m.setStatus("open " + url + ": " + err.Error())
			}
			return nil
		})
	}
	return nil
}

func parsePorts(s string) (remote, local int, err error) {
	r, l, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		l = r
	}
	remote, err1 := strconv.Atoi(r)
	local, err2 := strconv.Atoi(l)
	if err1 != nil || err2 != nil || remote < 1 || remote > 65535 || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("invalid port %q: want <port> or <port>:<local port>, each 1-65535", s)
	}
	return remote, local, nil
}

// startForward forwards box:remote to this computer's local port; when that's
// taken, it asks for another.
func (m *tui) startForward(box string, remote, local int) tea.Cmd {
	key := forwardKey(box, remote)
	if f := m.forwards[key]; f != nil {
		m.setStatus(box + ":" + strconv.Itoa(remote) + " is already at " + f.addr())
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local))
	if err != nil {
		m.input(fmt.Sprintf("localhost:%d is taken here. Forward %s:%d to which local port?", local, box, remote), "a free port", "", func(s string) tea.Cmd {
			l, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || l < 1 || l > 65535 {
				m.setStatus("invalid port " + strconv.Quote(s))
				return nil
			}
			return m.startForward(box, remote, l)
		})
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	f := &forward{box: box, remote: remote, local: local, ln: ln, cancel: cancel, started: time.Now()}
	m.forwards[key] = f
	go func() { <-ctx.Done(); ln.Close() }()
	go serveForward(ctx, m.sess, box, remote, ln)
	m.setStatus(fmt.Sprintf("%s → %s:%d (y copies it, O opens it)", f.addr(), box, remote))
	return nil
}

func (m *tui) stopForward(f *forward) {
	f.cancel()
	f.ln.Close()
	delete(m.forwards, forwardKey(f.box, f.remote))
}

func (m *tui) stopForwards() {
	for _, f := range m.forwards {
		m.stopForward(f)
	}
}

// restartForwards serves each forward over a new connection to the machine,
// on the same local ports.
func (m *tui) restartForwards() {
	old := m.forwards
	m.forwards = map[string]*forward{}
	for _, f := range old {
		f.cancel()
		f.ln.Close()
		m.startForward(f.box, f.remote, f.local)
	}
}

func openURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	return exec.Command("xdg-open", url).Start()
}

func (m *tui) portsView(w, h int) string {
	v := m.portv
	head := styleBold.Render("Ports") + styleDim.Render("  "+v.box.ID)
	var note string
	switch {
	case v.box.State != "running":
		note = v.box.ID + " is " + v.box.State + ": its listeners aren't read (that would wake it). a forwards a port you know; the box wakes on its first connection."
	case v.err != "":
		note = "couldn't read its listeners: " + v.err
	case v.read.IsZero():
		note = "reading its listeners…"
	default:
		note = "read " + ago(v.read) + "; an open port isn't proof a service is healthy"
	}
	lines := []string{head, styleDim.Render(truncate(note, w-1)), ""}
	rows := v.rows(m.forwards)
	v.cursor = max(min(v.cursor, len(rows)-1), 0)
	if len(rows) == 0 {
		lines = append(lines, styleDim.Render("  Nothing listening, and no forwards."))
	}
	for i, port := range rows {
		state := styleDim.Render("not forwarded")
		if f := m.forwards[forwardKey(v.box.ID, port)]; f != nil {
			state = styleRead.Render("→ " + f.addr())
		}
		where := ""
		for _, p := range v.ports {
			if p.Port == port && p.Loopback {
				where = styleDim.Render(" (box's loopback only)")
			}
		}
		line := spread(fmt.Sprintf("  %d", port)+where, state, w-1)
		if i == v.cursor {
			line = highlight(line, w-1, colorCursor)
		}
		lines = append(lines, line)
	}
	if others := m.otherForwards(v.box.ID); len(others) > 0 {
		lines = append(lines, "", styleLabel.Render("Forwards to other boxes"))
		lines = append(lines, others...)
	}
	lines = append(lines, "", styleDim.Render(truncate("enter forward/stop · a add a port · y copy · O open · r read again · esc", w-1)))
	return strings.Join(lines, "\n")
}

func (m *tui) otherForwards(box string) []string {
	var out []string
	for _, f := range m.forwards {
		if f.box != box {
			out = append(out, fmt.Sprintf("  %s → %s:%d", f.addr(), f.box, f.remote))
		}
	}
	slices.Sort(out)
	return out
}
