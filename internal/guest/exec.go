//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// credential runs a process as u with its supplementary groups (docker, say).
func credential(u *user.User) *syscall.Credential {
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	c := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if ids, err := u.GroupIds(); err == nil {
		for _, id := range ids {
			if g, err := strconv.ParseUint(id, 10, 32); err == nil {
				c.Groups = append(c.Groups, uint32(g))
			}
		}
	}
	return c
}

// userPath puts ~/.local/bin first: agents such as Claude Code install there.
func userPath(home string) string {
	return "PATH=" + home + "/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// lookPath resolves a command against the user's PATH, not hutch-guest's own.
func lookPath(name, home string) string {
	if strings.Contains(name, "/") {
		return name
	}
	for _, dir := range strings.Split(strings.TrimPrefix(userPath(home), "PATH="), ":") {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return name
}

func execute(ctx context.Context, e guestproto.Exec, stdin io.Reader, r *responder) (int, error) {
	name := e.User
	if name == "" {
		name = "dev"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}

	cmd := exec.CommandContext(ctx, lookPath(e.Argv[0], u.HomeDir), e.Argv[1:]...)
	cmd.Dir = e.Dir
	if cmd.Dir == "" {
		cmd.Dir = u.HomeDir
	}
	cmd.Env = append([]string{userPath(u.HomeDir), "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}, e.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: credential(u),
		Setpgid:    true,
	}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	// Plain pipes rather than StdoutPipe: background processes the command starts
	// inherit them, and the call must still end when the command itself exits.
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return 0, err
	}
	defer outR.Close()
	defer errR.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	// Input through a real pipe: with a plain reader, Wait would also wait for the
	// host's input to end, which may be never.
	var inW *os.File
	if stdin != nil {
		var inR *os.File
		if inR, inW, err = os.Pipe(); err != nil {
			return 0, err
		}
		defer inR.Close()
		cmd.Stdin = inR
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		if inW != nil {
			inW.Close()
		}
		return 0, err
	}
	if inW != nil {
		go func() {
			io.Copy(inW, stdin)
			inW.Close()
		}()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go stream(&wg, outR, guestproto.RespStdout, r)
	go stream(&wg, errR, guestproto.RespStderr, r)
	err = cmd.Wait()
	// Read what is already written, then stop if background processes keep the pipes open.
	deadline := time.Now().Add(200 * time.Millisecond)
	outR.SetReadDeadline(deadline)
	errR.SetReadDeadline(deadline)
	wg.Wait()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	default:
		return 0, fmt.Errorf("wait: %w", err)
	}
}

func stream(wg *sync.WaitGroup, src io.Reader, typ string, r *responder) {
	defer wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if r.send(guestproto.Response{Type: typ, Data: buf[:n]}) != nil {
				io.Copy(io.Discard, src)
				return
			}
		}
		if err != nil {
			return
		}
	}
}
