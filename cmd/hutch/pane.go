package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

// tmuxFeatures are what the pane's emulator supports; the box's tmux can't detect
// them from its answers, so its client declares them.
const tmuxFeatures = "RGB,strikethrough,usstyle,hyperlinks,title,focus,clipboard"

// A pane is one box terminal drawn in the TUI: a thread, in a tmux view session of
// its own grouped with the box's threads, or the box's shell. Its stream feeds an
// in-process emulator, which the TUI draws.
type pane struct {
	box     string
	n       int    // the thread
	session string // the thread's view session in the box's tmux

	mu  sync.Mutex // guards emu
	emu *vt.Emulator

	wmu  sync.Mutex // guards writes to conn
	conn net.Conn

	// in carries what the emulator writes (keys, and its answers to terminal
	// queries) to the stream, so typing never waits on the connection; ready
	// closes once the stream is up or has failed.
	in        chan []byte
	ready     chan struct{}
	readyOnce sync.Once

	ended  atomic.Bool
	err    atomic.Value // string: why it ended
	hidden atomic.Bool  // its view session is marked off screen
}

func paneKey(box string, n int) string { return box + "/" + strconv.Itoa(n) }

func newPane(box string, n, w, h int) *pane {
	b := make([]byte, 4)
	rand.Read(b)
	p := &pane{box: box, n: n, session: "view-" + hex.EncodeToString(b), in: make(chan []byte, 256), ready: make(chan struct{})}
	p.emu = vt.NewEmulator(w, h)
	p.emu.SetScrollbackSize(1) // tmux in the box keeps the history
	// The emulator's input is an unbuffered pipe: a key blocks until this reads it.
	go func() {
		defer close(p.in)
		buf := make([]byte, 32<<10)
		for {
			n, err := p.emu.Read(buf)
			if n > 0 {
				p.in <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		<-p.ready
		for data := range p.in {
			p.send(guestproto.Input{Type: guestproto.InputStdin, Data: data})
		}
	}()
	return p
}

func (p *pane) setReady() { p.readyOnce.Do(func() { close(p.ready) }) }

// argv attaches the pane's thread with the mouse on, so the wheel scrolls tmux's
// history.
func (p *pane) argv() []string {
	return []string{"tmux", "-T", tmuxFeatures, "new-session", "-t", agentSession, "-s", p.session,
		";", "set-option", "destroy-unattached", "on", ";", "set-option", "status", "off", ";", "set-option", "prefix", "None",
		";", "set-option", "mouse", "on", ";", "select-window", "-t", p.session + ":" + strconv.Itoa(p.n)}
}

// run connects the pane and copies its output into the emulator until the stream
// ends; changed is called after each write, clip with what the box copied.
func (p *pane) run(ctx context.Context, sess *yamux.Session, changed func(), clip func(string)) {
	defer changed()
	defer p.setReady()
	fail := func(err error) {
		p.err.Store(err.Error())
		p.ended.Store(true)
	}
	p.mu.Lock()
	w, h := p.emu.Width(), p.emu.Height()
	// tmux's selection reaches the laptop's clipboard as OSC 52: "c;<base64>".
	p.emu.RegisterOscHandler(52, func(data []byte) bool {
		if _, enc, ok := strings.Cut(string(data), ";"); ok {
			if text, err := base64.StdEncoding.DecodeString(enc); err == nil {
				clip(string(text))
			}
		}
		return true
	})
	p.mu.Unlock()
	conn, err := rpc.OpenStream(ctx, sess, api.BoxPTY, api.PTYParams{ID: p.box, Argv: p.argv(), User: "dev",
		Term: "xterm-256color", Rows: h, Cols: w})
	if err != nil {
		fail(err)
		return
	}
	defer conn.Close()
	var first guestproto.Response
	if err := wire.Read(conn, &first); err != nil {
		fail(err)
		return
	}
	if first.Type == guestproto.RespError {
		fail(fmt.Errorf("%s", first.Error))
		return
	}
	p.wmu.Lock()
	p.conn = conn
	p.wmu.Unlock()
	p.setReady()
	for {
		var r guestproto.Response
		if err := wire.Read(conn, &r); err != nil {
			if err == io.EOF {
				err = fmt.Errorf("disconnected")
			}
			fail(err)
			return
		}
		switch r.Type {
		case guestproto.RespStdout:
			p.mu.Lock()
			p.emu.Write(r.Data)
			p.mu.Unlock()
			changed()
		case guestproto.RespExit:
			fail(fmt.Errorf("ended"))
			return
		case guestproto.RespError:
			fail(fmt.Errorf("%s", r.Error))
			return
		}
	}
}

func (p *pane) send(in guestproto.Input) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.conn != nil {
		wire.Write(p.conn, in)
	}
}

// close ends the stream; the view session goes with its last client.
func (p *pane) close() {
	p.wmu.Lock()
	if p.conn != nil {
		p.conn.Close()
	}
	p.wmu.Unlock()
	p.emu.Close()
}

func (p *pane) resize(w, h int) {
	p.mu.Lock()
	same := p.emu.Width() == w && p.emu.Height() == h
	if !same {
		p.emu.Resize(w, h)
	}
	p.mu.Unlock()
	if !same {
		p.send(guestproto.Input{Type: guestproto.InputResize, Rows: h, Cols: w})
	}
}

// render is the screen as styled lines, and where the cursor is if it shows.
func (p *pane) render() (string, *uv.Position) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cur *uv.Position
	if !p.emu.CursorHidden() {
		c := p.emu.CursorPosition()
		cur = &c
	}
	return p.emu.Render(), cur
}

func (p *pane) withEmu(f func(*vt.Emulator)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p.emu)
}

// setHidden marks the thread's view session off screen or on, so the machine
// knows whether you see the thread (for unread).
func (p *pane) setHidden(ctx context.Context, sess *yamux.Session, hidden bool) {
	if p.hidden.Swap(hidden) == hidden {
		return
	}
	argv := []string{"tmux", "set-option", "-t", p.session, "-u", guestproto.HiddenOption}
	if hidden {
		argv = []string{"tmux", "set-option", "-t", p.session, guestproto.HiddenOption, "1"}
	}
	go rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: p.box, Argv: argv}, io.Discard, io.Discard)
}
