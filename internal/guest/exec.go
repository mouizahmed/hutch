//go:build linux

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

const defaultPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func execute(ctx context.Context, e guestproto.Exec, r *responder) (int, error) {
	name := e.User
	if name == "" {
		name = "dev"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)

	cmd := exec.CommandContext(ctx, e.Argv[0], e.Argv[1:]...)
	cmd.Dir = e.Dir
	if cmd.Dir == "" {
		cmd.Dir = u.HomeDir
	}
	cmd.Env = append([]string{defaultPath, "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}, e.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
		Setpgid:    true,
	}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go stream(&wg, stdout, guestproto.RespStdout, r)
	go stream(&wg, stderr, guestproto.RespStderr, r)
	wg.Wait()

	err = cmd.Wait()
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
