//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/durable"
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

func (w *Worker) gvisorCPUFeatures(version string) ([]string, error) {
	data, err := w.runsc(version, "cpu-features")
	if err != nil {
		return nil, fmt.Errorf("query gVisor CPU compatibility: %w", err)
	}
	features := strings.FieldsFunc(string(data), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	if len(features) == 0 {
		return nil, fmt.Errorf("gVisor reported no CPU features")
	}
	slices.Sort(features)
	return slices.Compact(features), nil
}

func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

func supportsFeatures(available, required []string) bool {
	if len(required) == 0 {
		return false // Older imported checkpoints have unknown requirements.
	}
	for _, feature := range required {
		if !slices.Contains(available, feature) {
			return false
		}
	}
	return true
}

// Local sleep/wake and forks remain on their original host. Imports need an
// additional feature check: two AMD (or Intel) hosts can have different SIMD.
// Unknown older imports keep their disk and cold boot; local legacy boxes keep
// their existing warm behavior.
func (w *Worker) restoresWarm(info *durable.BoxInfo) (bool, error) {
	if info.Archived || !w.warm(info.Class) {
		return false, nil
	}
	if info.GVisor == "" {
		return true, nil
	}
	if len(info.GVisorFeatures) == 0 {
		return false, nil
	}
	available, err := w.gvisorCPUFeatures(info.GVisor)
	if err != nil {
		log.Printf("restore cold: %v", err)
		return false, nil
	}
	return supportsFeatures(available, info.GVisorFeatures), nil
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

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (w *Worker) bundleDir(id string) string { return filepath.Join(w.boxDir(id), "bundle") }
func (w *Worker) mountDir(id string) string  { return filepath.Join(w.boxDir(id), "root") }

func sandboxID(id string) string { return fmt.Sprintf("H%x", sha256.Sum256([]byte(id))) }

func (b *Box) sandboxID() string {
	if b.RunscID != "" {
		return b.RunscID
	}
	return b.ID
}

// Legacy IDs may be prefixes of other boxes. Never pass an absent legacy name
// to runsc: its abbreviation lookup could target a different container.
func (w *Worker) legacySandboxExists(b *Box) (bool, error) {
	if _, err := os.Stat(runscRoot); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	out, err := w.runsc(b.GVisor, "list", "--quiet")
	if err != nil {
		return false, err
	}
	for _, id := range strings.Fields(string(out)) {
		if id == b.ID {
			return true, nil
		}
	}
	return false, nil
}

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
			"env":          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "TERM=xterm-256color", "HUTCH_GUEST_TOKEN=" + b.Token},
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
	if b.RunscID == "" {
		// A previous daemon may have stopped partway through a legacy start.
		// Exact existence is required before cleaning up its old OCI name.
		exists, err := w.legacySandboxExists(b)
		if err != nil {
			return err
		}
		if exists {
			if _, err := w.runsc(b.GVisor, "delete", "--force", b.ID); err != nil {
				return err
			}
		}
		b.RunscID = sandboxID(b.ID)
	}
	s, err := w.newNetwork(b.ID, b.Allow)
	if err != nil {
		return err
	}
	b.Slot = int(s)
	fail := func(err error) error {
		w.killSandbox(b)
		w.teardown(s)
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
	w.runsc(b.GVisor, "delete", "--force", b.sandboxID()) // left by a hutchd that stopped mid-start
	args := []string{"create", "--bundle", w.bundleDir(b.ID), b.sandboxID()}
	if from != "" {
		// Pages load in the background: the box answers before all its memory is in.
		args = []string{"restore", "--detach", "--background", "--bundle", w.bundleDir(b.ID), "--image-path", from, b.sandboxID()}
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
		if _, err := w.runsc(b.GVisor, "start", b.sandboxID()); err != nil {
			return fail(err)
		}
	}
	if b.PID, err = w.sandboxPID(b); err != nil {
		return fail(err)
	}
	if err := w.guestRestore(ctx, b, guestTimeout); err != nil {
		return fail(fmt.Errorf("guest: %w", err))
	}
	b.State, b.Checkpointed = Running, time.Now().UTC()
	if err := w.save(b); err != nil {
		return fail(err)
	}
	return nil
}

func (w *Worker) sandboxPID(b *Box) (int, error) {
	out, err := w.runsc(b.GVisor, "state", b.sandboxID())
	if err != nil {
		return 0, err
	}
	var st struct {
		PID int    `json:"pid"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(out, &st); err != nil || st.PID == 0 || st.ID != b.sandboxID() {
		return 0, fmt.Errorf("runsc state %s: no pid", b.ID)
	}
	return st.PID, nil
}

// killSandbox stops a box's sandbox and unmounts its disk.
func (w *Worker) killSandbox(b *Box) {
	exists := true
	if b.RunscID == "" {
		var err error
		if exists, err = w.legacySandboxExists(b); err != nil {
			// Killing by its exact ID is safe even if it's gone; leaving it
			// running while its box is removed isn't.
			log.Printf("%s: list sandboxes: %v; killing it anyway", b.ID, err)
			exists = true
		}
	}
	if exists {
		w.runsc(b.GVisor, "kill", b.sandboxID(), "KILL")
	}
	for i := 0; i < 200 && b.PID > 0 && processAlive(b.PID); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if exists {
		w.runsc(b.GVisor, "delete", "--force", b.sandboxID())
	}
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
	// Processes may have started on any host the box ran on, and gVisor can't
	// always hide a host's extra features from them: the snapshot needs every
	// feature of each. Unknown, it imports cold elsewhere.
	if features, err := w.gvisorCPUFeatures(b.GVisor); err == nil {
		b.GVisorFeatures = union(b.GVisorFeatures, features)
	} else {
		log.Printf("%s: snapshot without CPU features, so it restores cold on other machines: %v", b.ID, err)
		b.GVisorFeatures = nil
	}
	w.syncGuest(ctx, b)
	if _, err := w.runsc(b.GVisor, "pause", b.sandboxID()); err != nil {
		return err
	}
	resume := func(err error) error {
		w.runsc(b.GVisor, "resume", b.sandboxID())
		return err
	}
	if err := w.syncDisk(b.ID); err != nil {
		return resume(err)
	}
	args := []string{"checkpoint", "--image-path", gen}
	if leaveRunning {
		args = append(args, "--leave-running")
	}
	if _, err := w.runsc(b.GVisor, append(args, b.sandboxID())...); err != nil {
		return resume(err)
	}
	disk := filepath.Join(w.boxDir(b.ID), "rootfs.ext4")
	if leaveRunning {
		if err := clone(disk, filepath.Join(gen, "rootfs.ext4")); err != nil {
			return resume(err)
		}
		_, err := w.runsc(b.GVisor, "resume", b.sandboxID())
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
