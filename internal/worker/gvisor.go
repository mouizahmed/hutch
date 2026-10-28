//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// gVisor boxes, on machines without /dev/kvm: a box is a runsc sandbox whose root
// is its rootfs.ext4, loop-mounted on the host while it runs (the same disk a
// Firecracker box boots), in its own netns behind its slot's. Sleep is a gVisor
// checkpoint; hutch-guest is the sandbox's init and serves hutchd over TCP.

// The files of a gVisor checkpoint, as runsc writes them.
var checkpointFiles = []string{"checkpoint.img", "pages.img", "pages_meta.img"}

const runscRoot = "/run/hutch/runsc"

// runscFlags go on every runsc call for a box; they must agree across them. The
// root's own overlay is off (writes reach the box's disk), raw sockets are allowed
// (ping, Docker), setuid binaries such as sudo work, and a checkpoint of a running
// box keeps its connections (an agent's to its API) instead of resetting them.
var runscFlags = []string{"--root", runscRoot, "--overlay2=none", "--net-raw", "--allow-suid",
	"--allow-connected-on-save", "--host-settings=ignore"}

// specVersion counts changes to the sandbox spec (mounts, flags): gVisor restores a
// checkpoint only into the sandbox it was taken in, so a change is a new class.
const specVersion = 2

func gvisorClass(vendor, version string) string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	return fmt.Sprintf("%s-%s/gvisor-%s-s%d", arch, vendor, version, specVersion)
}

// gvisorVersions lists the installed gVisor versions, oldest first.
func (w *Worker) gvisorVersions() []string {
	entries, _ := os.ReadDir(filepath.Join(w.Root, "gvisor"))
	var out []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(w.Root, "gvisor", e.Name(), "runsc")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// guestToken is the machine's token for hutch-guest in gVisor boxes, made once.
func guestToken(root string) (string, error) {
	path := filepath.Join(root, "guest-token")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 32)
	rand.Read(b)
	t := hex.EncodeToString(b)
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

func (w *Worker) bundleDir(id string) string { return filepath.Join(w.boxDir(id), "bundle") }
func (w *Worker) mountDir(id string) string  { return filepath.Join(w.boxDir(id), "root") }

// runsc runs a runsc command for a box of gVisor version v, with the flags every
// call must agree on.
func (w *Worker) runsc(v string, args ...string) ([]byte, error) {
	full := append(append([]string{}, runscFlags...), args...)
	cmd := exec.Command(filepath.Join(w.Root, "gvisor", v, "runsc"), full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return out, fmt.Errorf("runsc %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

// mountDisk loop-mounts a box's disk as its sandbox's root.
func (w *Worker) mountDisk(id string) error {
	dir := w.mountDir(id)
	if mounted(dir) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return run("mount", "-o", "loop,noatime", filepath.Join(w.boxDir(id), "rootfs.ext4"), dir)
}

func (w *Worker) unmountDisk(id string) error {
	dir := w.mountDir(id)
	if !mounted(dir) {
		return nil
	}
	return run("umount", dir)
}

func mounted(dir string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 4 && fields[4] == dir {
			return true
		}
	}
	return false
}

// syncDisk writes a mounted disk's dirty data into its image file.
func (w *Worker) syncDisk(id string) error {
	f, err := os.Open(w.mountDir(id))
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

func (w *Worker) cgroupPath(id string) string { return "/hutch/" + id }

// writeSpec writes the sandbox's OCI spec: hutch-guest as init, the box's netns,
// its size as limits, and every capability, which stay inside gVisor.
func (w *Worker) writeSpec(b *Box, s slot) error {
	caps := []string{}
	for i := 0; i <= 40; i++ {
		caps = append(caps, capName(i))
	}
	tmpfs := func(dst, opts string) map[string]any {
		return map[string]any{"destination": dst, "type": "tmpfs", "source": "tmpfs", "options": strings.Split(opts, ",")}
	}
	spec := map[string]any{
		"ociVersion": "1.0.0",
		"process": map[string]any{
			"user":         map[string]any{"uid": 0, "gid": 0},
			"args":         []string{"/usr/local/bin/hutch-guest", "init", "--listen", fmt.Sprintf("tcp:0.0.0.0:%d", guestproto.Port), "--backend", "gvisor"},
			"env":          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm-256color", "HUTCH_GUEST_TOKEN=" + w.token},
			"cwd":          "/",
			"capabilities": map[string]any{"bounding": caps, "effective": caps, "permitted": caps, "inheritable": caps, "ambient": caps},
			"rlimits":      []map[string]any{{"type": "RLIMIT_NOFILE", "hard": 1048576, "soft": 1048576}},
		},
		"root":     map[string]any{"path": w.mountDir(b.ID), "readonly": false},
		"hostname": b.ID,
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			tmpfs("/dev", "nosuid,strictatime,mode=755,size=65536k"),
			{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			tmpfs("/dev/shm", "nosuid,noexec,nodev,mode=1777"),
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"nosuid", "noexec", "nodev"}},
			// gVisor's own cgroups, for Docker in the box; it can't mount cgroup2 itself.
			{"destination": "/sys/fs/cgroup", "type": "cgroup", "source": "cgroup", "options": []string{"nosuid", "noexec", "nodev"}},
			tmpfs("/run", "nosuid,nodev,mode=755"),
		},
		"linux": map[string]any{
			"namespaces": []map[string]any{{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
				{"type": "network", "path": s.boxNetnsPath()}},
			"cgroupsPath": w.cgroupPath(b.ID),
			"resources": map[string]any{
				"memory": map[string]any{"limit": int64(b.MemMiB) << 20},
				"cpu":    map[string]any{"quota": int64(b.VCPUs) * 100000, "period": 100000},
			},
		},
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.bundleDir(b.ID), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.bundleDir(b.ID), "config.json"), data, 0o644)
}

var capNames = []string{"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID", "SETPCAP",
	"LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "IPC_OWNER", "SYS_MODULE",
	"SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME",
	"SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG",
	"WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE"}

func capName(i int) string { return "CAP_" + capNames[i] }

// startSandbox sets up the box's network and disk and starts its sandbox: a cold
// boot, or a restore from the checkpoint in from. It then sends hutch-guest the
// restore message. On failure everything it created is undone.
func (w *Worker) startSandbox(ctx context.Context, b *Box, from string, guestTimeout time.Duration) error {
	s, err := w.newNetwork(b.Allow)
	if err != nil {
		return err
	}
	b.Slot = int(s)
	fail := func(err error) error {
		w.killSandbox(b)
		s.teardown()
		b.Slot = 0
		return err
	}
	if err := ensureCgroup(); err != nil {
		return fail(err)
	}
	if err := w.mountDisk(b.ID); err != nil {
		return fail(err)
	}
	if err := w.writeSpec(b, s); err != nil {
		return fail(err)
	}
	console, err := os.OpenFile(w.ConsolePath(b.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(console, "\n--- hutchd: starting %s at %s ---\n", b.ID, time.Now().UTC().Format(time.RFC3339))
	w.runsc(b.GVisor, "delete", "--force", b.ID) // left by a hutchd that stopped mid-start
	args := []string{"create", "--bundle", w.bundleDir(b.ID), b.ID}
	if from != "" {
		// Pages load in the background: the box answers before all its memory is in.
		args = []string{"restore", "--detach", "--background", "--bundle", w.bundleDir(b.ID), "--image-path", from, b.ID}
	}
	cmd := exec.Command(filepath.Join(w.Root, "gvisor", b.GVisor, "runsc"), append(append([]string{}, runscFlags...), args...)...)
	cmd.Stdout, cmd.Stderr = console, console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Run()
	console.Close()
	if err != nil {
		return fail(fmt.Errorf("runsc %s: %v (see hutch console %s)", args[0], err, b.ID))
	}
	if from == "" {
		if _, err := w.runsc(b.GVisor, "start", b.ID); err != nil {
			return fail(err)
		}
	}
	if b.PID, err = w.sandboxPID(b); err != nil {
		return fail(err)
	}
	b.State, b.Checkpointed = Running, time.Now().UTC()
	if err := w.save(b); err != nil {
		return fail(err)
	}
	if err := w.guestRestore(ctx, b, guestTimeout); err != nil {
		return fail(fmt.Errorf("guest: %w", err))
	}
	return nil
}

func (w *Worker) sandboxPID(b *Box) (int, error) {
	out, err := w.runsc(b.GVisor, "state", b.ID)
	if err != nil {
		return 0, err
	}
	var st struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(out, &st); err != nil || st.PID == 0 {
		return 0, fmt.Errorf("runsc state %s: no pid", b.ID)
	}
	return st.PID, nil
}

// killSandbox stops a box's sandbox and unmounts its disk.
func (w *Worker) killSandbox(b *Box) {
	w.runsc(b.GVisor, "kill", b.ID, "KILL")
	for i := 0; i < 200 && b.PID > 0 && processAlive(b.PID); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	w.runsc(b.GVisor, "delete", "--force", b.ID)
	b.PID = 0
	if err := w.unmountDisk(b.ID); err != nil {
		time.Sleep(200 * time.Millisecond)
		w.unmountDisk(b.ID)
	}
}

// checkpointSandbox pauses the sandbox and writes a checkpoint and a matching copy
// of the disk into gen. Sleeping (leaveRunning false), the sandbox stops and the
// disk is unmounted before it's copied; otherwise the sandbox resumes. On failure
// the sandbox is left running.
func (w *Worker) checkpointSandbox(ctx context.Context, b *Box, gen string, leaveRunning bool) error {
	w.syncGuest(ctx, b)
	if _, err := w.runsc(b.GVisor, "pause", b.ID); err != nil {
		return err
	}
	resume := func(err error) error {
		w.runsc(b.GVisor, "resume", b.ID)
		return err
	}
	if err := w.syncDisk(b.ID); err != nil {
		return resume(err)
	}
	args := []string{"checkpoint", "--image-path", gen}
	if leaveRunning {
		args = append(args, "--leave-running")
	}
	if _, err := w.runsc(b.GVisor, append(args, b.ID)...); err != nil {
		return resume(err)
	}
	disk := filepath.Join(w.boxDir(b.ID), "rootfs.ext4")
	if leaveRunning {
		if err := clone(disk, filepath.Join(gen, "rootfs.ext4")); err != nil {
			return resume(err)
		}
		_, err := w.runsc(b.GVisor, "resume", b.ID)
		return err
	}
	w.killSandbox(b)
	return clone(disk, filepath.Join(gen, "rootfs.ext4"))
}

// snapshotSandbox writes a gVisor box's next generation, like snapshotGen.
func (w *Worker) snapshotSandbox(ctx context.Context, b *Box, leaveRunning bool) (cleanup func(), err error) {
	prev := b.Generation
	gen := w.genDir(b.ID, prev+1)
	os.RemoveAll(gen)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return nil, err
	}
	if err := w.checkpointSandbox(ctx, b, gen, leaveRunning); err != nil {
		os.RemoveAll(gen)
		return nil, err
	}
	b.Generation, b.Checkpointed = prev+1, time.Now().UTC()
	return func() {
		if prev > 0 {
			os.RemoveAll(w.genDir(b.ID, prev))
		}
	}, nil
}

// ensureCgroup lets boxes' cgroups use the CPU and memory controllers.
func ensureCgroup() error {
	if err := os.MkdirAll("/sys/fs/cgroup/hutch", 0o755); err != nil {
		return err
	}
	for _, dir := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/hutch"} {
		os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0)
	}
	return nil
}

// cgroupStat reads one field of a box cgroup's stat file, or the whole file's
// number when field is "".
func (w *Worker) cgroupStat(b *Box, file, field string) (int64, error) {
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", w.cgroupPath(b.ID), file))
	if err != nil {
		return 0, err
	}
	if field == "" {
		return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, field+" "); ok {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, errors.New("no " + field + " in " + file)
}
