//go:build linux

package main

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A shell's view ends when the shell exits, instead of tmux showing another of
// the box's terminals in its place; another terminal closing leaves it be.
func TestShellViewEndsWithItsShell(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	// The view is a tmux client, which needs a terminal type (CI sets none).
	t.Setenv("TERM", "xterm-256color")
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("tmux", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	tmux("new-session", "-d", "-s", agentSession, "-n", "agent", "sleep 60")
	shell := make(chan struct{})
	tmux("new-window", "-t", agentSession, "-n", "shell", "read x; exit")
	tmux("new-window", "-t", agentSession, "-n", "other", "read x; exit")
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })
	n := strings.TrimSpace(tmux("display-message", "-p", "-t", agentSession+":shell", "#{window_index}"))

	argv := shellViewArgv(atoi(t, n))
	view := argv[len(argv)-1][:strings.Index(argv[len(argv)-1], ":")]
	cmd := exec.Command(argv[0], argv[1:]...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var mu sync.Mutex
	var screen []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			mu.Lock()
			screen = append(screen, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("the view's terminal: %q", screen)
			mu.Unlock()
		}
	})
	go func() { cmd.Wait(); close(shell) }()
	waitFor(t, func() bool { return exec.Command("tmux", "has-session", "-t", view).Run() == nil }, "the view to start")

	tmux("send-keys", "-t", agentSession+":other", "x", "Enter")
	time.Sleep(500 * time.Millisecond)
	select {
	case <-shell:
		t.Fatal("the view ended when another terminal closed")
	default:
	}

	tmux("send-keys", "-t", agentSession+":shell", "x", "Enter")
	select {
	case <-shell:
	case <-time.After(5 * time.Second):
		t.Fatal("the view didn't end when its shell exited")
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
