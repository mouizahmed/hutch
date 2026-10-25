//go:build compat

// Package compat tests the current host client against every hutch-guest build
// listed in compat/guests.txt, plus the current one: a box asleep for months wakes
// with its old guest still running and must keep working with a new hutchd.
//
// It runs as root in a throwaway container (make compat, or the CI compat job):
// the guest sets the hostname and runs commands as user dev.
package compat

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/wire"
)

func repoRoot(t *testing.T) string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func guestRefs(t *testing.T, root string) []string {
	f, err := os.Open(filepath.Join(root, "compat", "guests.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	refs := []string{"current"}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			refs = append(refs, strings.Fields(line)[0])
		}
	}
	return refs
}

// buildGuest builds hutch-guest from the working tree ("current") or a git ref.
func buildGuest(t *testing.T, root, ref, dir string) string {
	src := root
	if ref != "current" {
		src = filepath.Join(dir, "src")
		if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", src, ref).CombinedOutput(); err != nil {
			t.Fatalf("worktree %s: %v: %s", ref, err, out)
		}
		t.Cleanup(func() { exec.Command("git", "-C", root, "worktree", "remove", "--force", src).Run() })
	}
	bin := filepath.Join(dir, "hutch-guest")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/hutch-guest")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", ref, err, out)
	}
	return bin
}

// fakeFirecracker accepts host connections on hostPath, answers Firecracker's
// "CONNECT <port>" handshake, and splices them to the guest's Unix socket.
func fakeFirecracker(t *testing.T, hostPath, guestPath string) {
	ln, err := net.Listen("unix", hostPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(io.LimitReader(c, 64)).ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "CONNECT ") {
					return
				}
				g, err := net.Dial("unix", guestPath)
				if err != nil {
					return
				}
				defer g.Close()
				io.WriteString(c, "OK 1\n")
				done := make(chan struct{}, 2)
				go func() { io.Copy(g, c); done <- struct{}{} }()
				go func() { io.Copy(c, g); done <- struct{}{} }()
				<-done
			}()
		}
	}()
}

func TestGuestCompat(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a throwaway container: make compat")
	}
	root := repoRoot(t)
	for _, ref := range guestRefs(t, root) {
		t.Run(ref, func(t *testing.T) {
			dir := t.TempDir()
			bin := buildGuest(t, root, ref, dir)
			guestSock, hostSock := filepath.Join(dir, "g.sock"), filepath.Join(dir, "v.sock")

			ctx, cancel := context.WithCancel(context.Background())
			guest := exec.CommandContext(ctx, bin, "serve", "--listen", "unix:"+guestSock)
			guest.Stderr = os.Stderr
			if err := guest.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() { cancel(); guest.Wait() }()
			for i := 0; ; i++ {
				if _, err := os.Stat(guestSock); err == nil {
					break
				}
				if i > 200 {
					t.Fatal("guest did not listen")
				}
				time.Sleep(10 * time.Millisecond)
			}
			fakeFirecracker(t, hostSock, guestSock)
			checkGuest(t, &guestproto.Client{UDSPath: hostSock}, ref)
		})
	}
}

func checkGuest(t *testing.T, c *guestproto.Client, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hello, err := c.Hello(ctx)
	if err != nil || hello.Version < 1 {
		t.Fatalf("hello: %+v %v", hello, err)
	}

	// No clock change: the container shares the runner's clock.
	box := "compat-" + strings.ReplaceAll(ref, "/", "-")
	if len(box) > 40 {
		box = box[:40]
	}
	if err := c.Restore(ctx, guestproto.Restore{BoxID: box, Hostname: "compat-box"}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	var out, errb bytes.Buffer
	code, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"sh", "-c", "hostname; id -un; echo err >&2; exit 3"}, User: "dev"}, &out, &errb)
	if err != nil || code != 3 || out.String() != "compat-box\ndev\n" || errb.String() != "err\n" {
		t.Fatalf("exec: code=%d err=%v stdout=%q stderr=%q", code, err, out.String(), errb.String())
	}

	if err := checkPTY(ctx, c); err != nil {
		t.Fatalf("pty: %v", err)
	}

	// Requests added after a guest was built must fail cleanly, never hang.
	if _, err := c.Status(ctx); err != nil && !strings.Contains(err.Error(), "unsupported request type") {
		t.Fatalf("status: %v", err)
	}
}

func checkPTY(ctx context.Context, c *guestproto.Client) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := guestproto.Request{Type: guestproto.TypePTY, Version: guestproto.Version,
		PTY: &guestproto.PTY{Argv: []string{"sh", "-c", "read x; echo got:$x"}, User: "dev", Rows: 24, Cols: 80}}
	if err := wire.Write(conn, req); err != nil {
		return err
	}
	var first guestproto.Response
	if err := wire.Read(conn, &first); err != nil {
		return err
	}
	if first.Type != guestproto.RespOK {
		return fmt.Errorf("first frame %+v", first)
	}
	if err := wire.Write(conn, guestproto.Input{Type: guestproto.InputStdin, Data: []byte("hi\r")}); err != nil {
		return err
	}
	var screen bytes.Buffer
	for {
		var r guestproto.Response
		if err := wire.Read(conn, &r); err != nil {
			return fmt.Errorf("read after %q: %w", screen.String(), err)
		}
		switch r.Type {
		case guestproto.RespStdout:
			screen.Write(r.Data)
		case guestproto.RespExit:
			if !strings.Contains(screen.String(), "got:hi") {
				return fmt.Errorf("screen %q", screen.String())
			}
			if r.ExitCode != 0 {
				return fmt.Errorf("exit %d", r.ExitCode)
			}
			return nil
		case guestproto.RespError:
			return errors.New(r.Error)
		}
	}
}
