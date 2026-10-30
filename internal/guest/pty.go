//go:build linux

package guest

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/wire"
)

func runPTY(conn net.Conn, p guestproto.PTY) error {
	name := p.User
	if name == "" {
		name = "dev"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	term := p.Term
	if !hasTerminfo(term) {
		term = "xterm-256color"
	}

	cmd := exec.Command(lookPath(p.Argv[0], u.HomeDir), p.Argv[1:]...)
	cmd.Dir = p.Dir
	if cmd.Dir == "" {
		cmd.Dir = u.HomeDir
	}
	cmd.Env = append([]string{userPath(u.HomeDir), "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username,
		"SHELL=/bin/bash", "TERM=" + term, "LANG=C.UTF-8"}, append(machineEnv(), p.Env...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential(u)}

	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(p.Rows), Cols: uint16(p.Cols)})
	if err != nil {
		return err
	}
	defer tty.Close()

	r := &responder{conn: conn}
	if err := r.send(guestproto.Response{Type: guestproto.RespOK}); err != nil {
		cmd.Process.Kill()
		return nil
	}

	// Host input; when the host disconnects, hang up the session.
	go func() {
		for {
			var in guestproto.Input
			if err := wire.Read(conn, &in); err != nil {
				cmd.Process.Signal(syscall.SIGHUP)
				return
			}
			switch in.Type {
			case guestproto.InputStdin:
				tty.Write(in.Data)
			case guestproto.InputResize:
				pty.Setsize(tty, &pty.Winsize{Rows: uint16(in.Rows), Cols: uint16(in.Cols)})
			}
		}
	}()

	output := make(chan struct{})
	go func() {
		defer close(output)
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 && r.send(guestproto.Response{Type: guestproto.RespStdout, Data: buf[:n]}) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()

	err = cmd.Wait()
	// Drain output the process wrote before exiting; a background job still holding
	// the tty must not keep the session open.
	select {
	case <-output:
	case <-time.After(time.Second):
		tty.Close()
		<-output
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	}
	r.send(guestproto.Response{Type: guestproto.RespExit, ExitCode: code})
	return nil
}

// hasTerminfo reports whether the guest can describe the client's terminal; newer
// terminals (Ghostty, kitty, WezTerm) are often missing, and tmux refuses to start.
func hasTerminfo(term string) bool {
	if term == "" || strings.ContainsAny(term, "/.") {
		return false
	}
	for _, dir := range []string{"/etc/terminfo", "/lib/terminfo", "/usr/share/terminfo"} {
		if _, err := os.Stat(filepath.Join(dir, term[:1], term)); err == nil {
			return true
		}
	}
	return false
}
