//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// sessions counts open exec and terminal requests.
var sessions atomic.Int64

const threadSession = "hutch"

func status(p guestproto.StatusParams) (guestproto.Status, error) {
	st := guestproto.Status{Sessions: int(sessions.Load())}
	viewed := map[int]bool{}
	typed := map[int]int64{}
	// Clients of the threads' session or a view grouped with it look at a window.
	if out, err := tmuxAsDev("list-clients", "-F", "#{session_name}|#{session_group}|#{window_index}|#{"+guestproto.HiddenOption+"}|#{client_activity}"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(line, "|")
			if len(f) != 5 {
				continue
			}
			st.Clients++
			if f[3] != guestproto.HiddenOff {
				st.Viewers++
			}
			if f[0] == threadSession || f[1] == threadSession {
				n, _ := strconv.Atoi(f[2])
				viewed[n] = viewed[n] || f[3] == ""
				// tmux sets a client's activity when it's typed in, not when it draws.
				at, _ := strconv.ParseInt(f[4], 10, 64)
				typed[n] = max(typed[n], at)
			}
		}
	}
	// The box's tmux.conf records when a window last rang the bell or was resized.
	fields := []string{"#{window_index}", "#{pane_dead}", "#{window_activity}", "#{@hutch-bell}", "#{@hutch-resized}",
		"#{" + guestproto.OptAgent + "}", "#{" + guestproto.OptAutonomy + "}", "#{" + guestproto.OptPrompt + "}",
		"#{" + guestproto.OptName + "}", "#{" + guestproto.OptCreated + "}", "#{pane_title}", "#{" + guestproto.OptLast + "}"}
	out, err := tmuxAsDev("list-windows", "-t", threadSession, "-F", strings.Join(fields, sep))
	saved := savedThreads{}
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(line, sep)
			if len(f) != len(fields) {
				continue
			}
			n, _ := strconv.Atoi(f[0])
			last, _ := strconv.ParseInt(f[2], 10, 64)
			bell, _ := strconv.ParseInt(f[3], 10, 64)
			resized, _ := strconv.ParseInt(f[4], 10, 64)
			created, _ := strconv.ParseInt(f[9], 10, 64)
			saved.Last, _ = strconv.Atoi(f[11])
			t := guestproto.ThreadStatus{N: n, Dead: f[1] == "1", LastOutput: last, LastInput: typed[n],
				Bell: bell > 0 && bell >= last, BellAt: bell, Redraw: resized > 0 && last <= resized+1, Viewed: viewed[n],
				Agent: f[5], Autonomy: f[6], Prompt: f[7], Name: f[8], Created: created, Title: f[10]}
			st.Threads = append(st.Threads, t)
			if t.Agent != "" {
				saved.Threads = append(saved.Threads, t)
			}
		}
		saveThreads(saved)
	}
	st.Ports = listeningPorts()
	st.Files = hashFiles(p.Watch)
	if p.Git {
		st.Git = gitStatus()
	}
	return st, nil
}

// gitStatus reads /workspace's repository, or nil when it has none.
func gitStatus() *guestproto.GitStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := asDev(ctx, "/workspace", "sh", "-c", `git rev-parse --git-dir >/dev/null 2>&1 || exit 3
git symbolic-ref --short -q HEAD || echo
git status --porcelain | wc -l
git rev-list --count HEAD --not --remotes 2>/dev/null || echo 0`)
	if err != nil {
		return nil
	}
	f := strings.Split(strings.TrimSpace(out), "\n")
	if len(f) != 3 {
		return nil
	}
	g := &guestproto.GitStatus{Branch: strings.TrimSpace(f[0])}
	g.Changes, _ = strconv.Atoi(strings.TrimSpace(f[1]))
	g.Unpushed, _ = strconv.Atoi(strings.TrimSpace(f[2]))
	return g
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
	return asDev(context.Background(), "", "tmux", args...)
}

// asDev runs a command as user dev and returns its stdout; on failure the error
// includes stderr.
func asDev(ctx context.Context, dir, name string, args ...string) (string, error) {
	u, err := user.Lookup("dev")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, lookPath(name, u.HomeDir), args...)
	cmd.Dir = dir
	cmd.Env = []string{userPath(u.HomeDir), "HOME=" + u.HomeDir, "USER=dev"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential(u)}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("%s", msg)
		}
		return out.String(), err
	}
	return out.String(), nil
}
