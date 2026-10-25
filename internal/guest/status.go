//go:build linux

package guest

import (
	"bytes"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// sessions counts open exec and terminal requests.
var sessions atomic.Int64

const threadSession = "hutch"

func status() (guestproto.Status, error) {
	st := guestproto.Status{Sessions: int(sessions.Load())}
	out, err := tmuxAsDev("list-windows", "-t", threadSession, "-F", "#{window_index} #{pane_dead} #{window_activity}")
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				continue
			}
			n, _ := strconv.Atoi(f[0])
			last, _ := strconv.ParseInt(f[2], 10, 64)
			st.Threads = append(st.Threads, guestproto.ThreadStatus{N: n, Dead: f[1] == "1", LastOutput: last})
		}
	}
	if out, err := tmuxAsDev("list-clients", "-F", "#{client_name}"); err == nil {
		st.Clients = len(strings.Fields(out))
	}
	return st, nil
}

// tmuxAsDev queries the tmux server of user dev; an error usually means none is running.
func tmuxAsDev(args ...string) (string, error) {
	u, err := user.Lookup("dev")
	if err != nil {
		return "", err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	cmd := exec.Command(lookPath("tmux", u.HomeDir), args...)
	cmd.Env = []string{userPath(u.HomeDir), "HOME=" + u.HomeDir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	return out.String(), err
}
