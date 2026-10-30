package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

// The box's agents live in the tmux session "hutch"; each `hutch shell` is its own
// session, shell-1, shell-2, ...
const agentSession = "hutch"

func sessionCommands() []*cobra.Command {
	attach := &cobra.Command{
		Use:     "attach <box>[/<thread>]",
		Short:   "Attach to a box's threads (wakes it if asleep)",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			box, n, err := parseTarget(args[0])
			if err != nil {
				return err
			}
			argv := []string{"tmux", "new-session", "-A", "-s", agentSession}
			if n > 0 {
				argv = viewArgv(n)
				// An agent that exited comes back through its picker.
				if b, err := findBox(ctx, sess, box); err == nil {
					for _, t := range b.Threads {
						if t.N == n && t.Agent != "" && t.State == api.ThreadExited {
							if err := rpc.Call(ctx, sess, api.ThreadResume, api.ThreadTarget{Box: box, N: n}, nil); err != nil {
								return err
							}
						}
					}
				}
			}
			return terminal(ctx, sess, box, argv)
		}),
	}

	shell := &cobra.Command{
		Use:     "shell <box>",
		Short:   "Open a thread running bash in a box, and attach to it",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			return openShell(ctx, sess, args[0])
		}),
	}
	return []*cobra.Command{attach, shell}
}

// openShell starts a thread running bash in a box and attaches to it; the thread
// closes when bash exits.
func openShell(ctx context.Context, sess *yamux.Session, box string) error {
	var t api.Thread
	if err := rpc.Call(ctx, sess, api.ThreadNew, api.ThreadParams{Box: box, Cmd: "bash"}, &t); err != nil {
		return err
	}
	return terminal(ctx, sess, box, viewArgv(t.N))
}

// terminal runs argv on a pseudo-terminal in the box, connected to this terminal.
func terminal(ctx context.Context, sess *yamux.Session, box string, argv []string) error {
	return connectTerminal(box, "", func(rows, cols int) (net.Conn, error) {
		return rpc.OpenStream(ctx, sess, api.BoxPTY, api.PTYParams{ID: box, Argv: argv, User: "dev",
			Term: os.Getenv("TERM"), Rows: rows, Cols: cols})
	})
}

// serialConsole connects this terminal to a running box's serial console.
func serialConsole(ctx context.Context, sess *yamux.Session, box string) error {
	banner := fmt.Sprintf("Connected to %s's serial console. Press Enter for a prompt; Enter ~ . quits.", box)
	return connectTerminal(box, banner, func(int, int) (net.Conn, error) {
		return rpc.OpenStream(ctx, sess, api.BoxSerial, api.ConsoleParams{ID: box})
	})
}

// escape filters keystrokes for a serial console, which has no exit of its own: "~."
// at the start of a line quits, as in ssh, and "~~" sends one "~". Hutch's local tmux
// already binds Ctrl-] and Ctrl-\, so those can't be the escape.
type escape struct {
	lineStart, tilde bool
}

func (e *escape) filter(in []byte) (out []byte, quit bool) {
	for _, b := range in {
		switch {
		case e.tilde:
			e.tilde = false
			if b == '.' {
				return out, true
			}
			out = append(out, '~')
			if b == '~' {
				continue
			}
		case e.lineStart && b == '~':
			e.tilde = true
			continue
		}
		out = append(out, b)
		e.lineStart = b == '\r' || b == '\n'
	}
	return out, false
}

// connectTerminal relays a terminal stream that speaks guestproto Input and Response
// frames. A banner means a serial console: it is printed on connect and "~." quits.
func connectTerminal(box, banner string, open func(rows, cols int) (net.Conn, error)) error {
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		return errors.New("needs an interactive terminal")
	}
	cols, rows, err := term.GetSize(out)
	if err != nil {
		return err
	}
	conn, err := open(rows, cols)
	if err != nil {
		return err
	}
	defer conn.Close()

	var first guestproto.Response
	if err := wire.Read(conn, &first); err != nil {
		return fmt.Errorf("open terminal: %w", err)
	}
	if first.Type == guestproto.RespError {
		return errors.New(first.Error)
	}
	if banner != "" {
		fmt.Fprintln(os.Stderr, banner)
	}

	old, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	defer term.Restore(in, old)

	var mu sync.Mutex
	send := func(i guestproto.Input) error {
		mu.Lock()
		defer mu.Unlock()
		return wire.Write(conn, i)
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(out); err == nil {
				send(guestproto.Input{Type: guestproto.InputResize, Rows: r, Cols: c})
			}
		}
	}()
	var quit atomic.Bool
	esc := &escape{lineStart: true}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := os.Stdin.Read(buf)
			data := append([]byte(nil), buf[:n]...)
			if banner != "" {
				var q bool
				data, q = esc.filter(data)
				quit.Store(q)
			}
			if len(data) > 0 && send(guestproto.Input{Type: guestproto.InputStdin, Data: data}) != nil {
				return
			}
			if quit.Load() {
				conn.Close()
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		var r guestproto.Response
		if err := wire.Read(conn, &r); err != nil {
			term.Restore(in, old)
			if quit.Load() {
				fmt.Fprintln(os.Stderr)
				return nil
			}
			return fmt.Errorf("connection to %s lost: %w", box, err)
		}
		switch r.Type {
		case guestproto.RespStdout:
			os.Stdout.Write(r.Data)
		case guestproto.RespExit:
			if r.ExitCode != 0 {
				term.Restore(in, old)
				return exitError(r.ExitCode)
			}
			return nil
		case guestproto.RespError:
			term.Restore(in, old)
			return errors.New(r.Error)
		}
	}
}

// viewArgv attaches a thread in a session of its own grouped with the box's
// "hutch" session, so several viewers can each show a different thread.
func viewArgv(n int) []string {
	b := make([]byte, 4)
	rand.Read(b)
	name := "view-" + hex.EncodeToString(b)
	return []string{"tmux", "new-session", "-t", agentSession, "-s", name,
		";", "set-option", "destroy-unattached", "on", ";", "select-window", "-t", name + ":" + strconv.Itoa(n)}
}
