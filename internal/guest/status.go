//go:build linux

package guest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
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

func status(watch []string) (guestproto.Status, error) {
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
	st.Ports = listeningPorts()
	st.Files = hashFiles(watch)
	return st, nil
}

// hashFiles hashes the watched files that exist; "~/" means dev's home.
func hashFiles(paths []string) map[string]string {
	if len(paths) == 0 {
		return nil
	}
	home := "/home/dev"
	if u, err := user.Lookup("dev"); err == nil {
		home = u.HomeDir
	}
	out := map[string]string{}
	for _, p := range paths {
		full := p
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			full = home + "/" + rest
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out[p] = hex.EncodeToString(sum[:])
	}
	return out
}

// listeningPorts reads TCP sockets in LISTEN state from /proc/net/tcp{,6}.
func listeningPorts() []guestproto.ListenPort {
	seen := map[int]*guestproto.ListenPort{}
	var ports []guestproto.ListenPort
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 4 || f[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			addr, portHex, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(portHex, 16, 32)
			if err != nil {
				continue
			}
			loopback := addr == "0100007F" || addr == "00000000000000000000000001000000"
			if p := seen[int(port)]; p != nil {
				p.Loopback = p.Loopback && loopback
				continue
			}
			ports = append(ports, guestproto.ListenPort{Port: int(port), Loopback: loopback})
			seen[int(port)] = &ports[len(ports)-1]
		}
	}
	return ports
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
