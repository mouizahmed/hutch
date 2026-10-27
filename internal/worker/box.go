//go:build linux

package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/firecracker"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// Boot creates a box from an image and cold boots it. The image's disk is grown to
// diskBytes when that is larger.
func (w *Worker) Boot(ctx context.Context, id, image string, vcpus, memMiB int, diskBytes int64, idle Idle, allow []string, store *Store) (*Box, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid box id %q", id)
	}
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := os.Mkdir(w.boxDir(id), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("box %q already exists", id)
		}
		return nil, err
	}
	b := &Box{ID: id, Image: image, VCPUs: vcpus, MemMiB: memMiB, FCVersion: w.FCVersion, Class: w.Class(), Created: time.Now().UTC(), Idle: idle, Allow: allow, Store: store}
	rootfs := filepath.Join(w.boxDir(id), "rootfs.ext4")
	if err := clone(filepath.Join(w.imageDir(image), "rootfs.ext4"), rootfs); err != nil {
		os.RemoveAll(w.boxDir(id))
		return nil, err
	}
	if err := growDisk(rootfs, diskBytes); err != nil {
		os.RemoveAll(w.boxDir(id))
		return nil, err
	}
	err = w.start(ctx, b, map[string]string{
		jailKernel: filepath.Join(w.imageDir(image), "vmlinux"),
		jailRootfs: filepath.Join(w.boxDir(id), "rootfs.ext4"),
	}, func(m *vm) error { return m.coldBoot(ctx, w.vendor) }, 60*time.Second)
	if err != nil {
		b.State, b.PID, b.Slot = Failed, 0, 0
		w.save(b)
		return b, fmt.Errorf("%w (box kept as failed; see its console)", err)
	}
	return b, nil
}

// growDisk extends a sparse ext4 image and its filesystem, offline.
func growDisk(path string, size int64) error {
	fi, err := os.Stat(path)
	if err != nil || size <= fi.Size() {
		return err
	}
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	// A snapshot's disk was never unmounted: e2fsck replays its journal and exits 1
	// (errors corrected) or 2; only 4 and up are failures.
	if out, err := exec.Command("e2fsck", "-fp", path).CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() >= 4 {
			return fmt.Errorf("e2fsck %s: %v: %s", path, err, out)
		}
	}
	return run("resize2fs", "-f", path)
}

// Rename gives a sleeping box a new id and bucket place; template versions are
// frozen this way.
func (w *Worker) Rename(id, newID string, store *Store) (*Box, error) {
	if !validID.MatchString(newID) {
		return nil, fmt.Errorf("invalid box id %q", newID)
	}
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	unlockNew, err := w.lock(newID)
	if err != nil {
		return nil, err
	}
	defer unlockNew()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Asleep {
		return nil, fmt.Errorf("box %q must be asleep to rename it", id)
	}
	if _, err := os.Stat(w.boxDir(newID)); err == nil {
		return nil, fmt.Errorf("box %q already exists", newID)
	}
	if err := os.Rename(w.boxDir(id), w.boxDir(newID)); err != nil {
		return nil, err
	}
	b.ID, b.Store, b.Uploaded = newID, store, 0
	if err := w.save(b); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(w.Root, "boxes", "."+id+".lock"))
	return b, nil
}

// Fork creates a box from a sleeping box's latest snapshot: the disk is reflinked and
// the memory file is shared copy-on-write until the new box writes to it.
func (w *Worker) Fork(ctx context.Context, srcID, id string, idle Idle, allow []string, store *Store) (*Box, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid box id %q", id)
	}
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := os.Mkdir(w.boxDir(id), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("box %q already exists", id)
		}
		return nil, err
	}
	b, err := w.copyFromSnapshot(srcID, id)
	if err == nil {
		b.Idle, b.Allow, b.Store = idle, allow, store
		err = w.start(ctx, b, map[string]string{
			jailRootfs: filepath.Join(w.boxDir(id), "rootfs.ext4"),
			jailState:  filepath.Join(w.boxDir(id), "base.vmstate"),
			jailMem:    b.BaseMem,
		}, func(m *vm) error { return m.restore(ctx) }, 10*time.Second)
	}
	if err != nil {
		os.RemoveAll(w.boxDir(id))
		return nil, err
	}
	return b, nil
}

// copyFromSnapshot holds the source box's lock only while its files are copied
// (reflink) and linked, so many forks of one box can start in parallel.
func (w *Worker) copyFromSnapshot(srcID, id string) (*Box, error) {
	unlock, err := w.lock(srcID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	src, err := w.load(srcID)
	if err != nil {
		return nil, err
	}
	if src.State != Asleep {
		return nil, fmt.Errorf("box %q must be asleep to fork it", srcID)
	}
	if !w.warm(src.Class) {
		return nil, fmt.Errorf("box %q has snapshot class %s, this worker runs %s", srcID, src.Class, strings.Join(w.Classes(), ", "))
	}
	gen := w.genDir(srcID, src.Generation)
	dir := w.boxDir(id)
	if err := clone(filepath.Join(w.boxDir(srcID), "rootfs.ext4"), filepath.Join(dir, "rootfs.ext4")); err != nil {
		return nil, err
	}
	if err := os.Link(filepath.Join(gen, "mem"), filepath.Join(dir, "base.mem")); err != nil {
		return nil, err
	}
	if err := os.Link(filepath.Join(gen, "vmstate"), filepath.Join(dir, "base.vmstate")); err != nil {
		return nil, err
	}
	// The source's stored chunks are the fork's until it changes them, so its first
	// upload compares against them.
	if src.Store != nil && src.Uploaded == src.Generation {
		if err := cloneDir(w.refDir(srcID), w.refDir(id)); err != nil {
			os.RemoveAll(w.refDir(id))
		}
	}
	return &Box{ID: id, Image: src.Image, VCPUs: src.VCPUs, MemMiB: src.MemMiB, FCVersion: src.FCVersion, Class: src.Class,
		Created: time.Now().UTC(), BaseMem: filepath.Join(dir, "base.mem")}, nil
}

// Wake restores a sleeping box from its latest snapshot.
func (w *Worker) Wake(ctx context.Context, id string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State == Archived {
		return w.coldWake(ctx, b)
	}
	if b.State != Asleep {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	if !w.warm(b.Class) {
		return nil, fmt.Errorf("box %q has snapshot class %s, this worker runs %s; it needs a cold boot", id, b.Class, strings.Join(w.Classes(), ", "))
	}
	gen := w.genDir(id, b.Generation)
	b.BaseMem = filepath.Join(gen, "mem")
	err = w.start(ctx, b, map[string]string{
		jailRootfs: filepath.Join(w.boxDir(id), "rootfs.ext4"),
		jailState:  filepath.Join(gen, "vmstate"),
		jailMem:    b.BaseMem,
	}, func(m *vm) error { return m.restore(ctx) }, 10*time.Second)
	if err != nil {
		b.State, b.PID, b.BaseMem = Asleep, 0, ""
		w.save(b)
		return nil, err
	}
	return b, nil
}

// start sets up the network and jail, runs boot (cold boot or restore), then sends
// hutch-guest the restore message. On failure everything it created is undone.
func (w *Worker) start(ctx context.Context, b *Box, links map[string]string, boot func(*vm) error, guestTimeout time.Duration) error {
	s, err := w.newNetwork(b.Allow)
	if err != nil {
		return err
	}
	b.Slot = int(s)
	fail := func(err error) error {
		w.killFirecracker(b)
		s.teardown()
		b.Slot = 0
		return err
	}
	m, err := w.startJailer(ctx, b, s, links)
	if err != nil {
		return fail(err)
	}
	if err := boot(m); err != nil {
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

func (w *Worker) guestRestore(ctx context.Context, b *Box, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := &guestproto.Client{UDSPath: w.vsockPath(b.ID)}
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 2*time.Second)
		_, err := c.Hello(attempt)
		cancelAttempt()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no answer from hutch-guest: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	seed := make([]byte, 32)
	rand.Read(seed)
	return c.Restore(ctx, guestproto.Restore{UnixNano: time.Now().UnixNano(), BoxID: b.ID, Hostname: b.ID, Seed: seed})
}

// Sleep snapshots a running box and stops its VM.
func (w *Worker) Sleep(ctx context.Context, id string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Running {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	cleanup, err := w.snapshotGen(ctx, b, w.running(b))
	if err != nil {
		return nil, err
	}
	w.killFirecracker(b)
	slot(b.Slot).teardown()
	b.State, b.Slot, b.BaseMem = Asleep, 0, ""
	if err := w.save(b); err != nil {
		return nil, err
	}
	cleanup()
	os.Remove(filepath.Join(w.boxDir(id), "base.mem"))
	os.Remove(filepath.Join(w.boxDir(id), "base.vmstate"))
	return b, nil
}

// Checkpoint snapshots a running box and lets it run on: a brief pause.
func (w *Worker) Checkpoint(ctx context.Context, id string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Running {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	m := w.running(b)
	cleanup, err := w.snapshotGen(ctx, b, m)
	if err != nil {
		return nil, err
	}
	if err := m.api.Resume(ctx); err != nil {
		return nil, fmt.Errorf("resume after checkpoint: %w", err)
	}
	// The next diff is relative to this snapshot.
	b.BaseMem = filepath.Join(w.genDir(id, b.Generation), "mem")
	if err := w.save(b); err != nil {
		return nil, err
	}
	cleanup()
	return b, nil
}

// syncGuest flushes the guest's page cache to its disk, so the disk alone is a
// consistent copy: a cold boot from it (archive, a move to another snapshot class)
// must not lose the last writes. A guest that doesn't answer is snapshotted anyway.
func (w *Worker) syncGuest(ctx context.Context, b *Box) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	(&guestproto.Client{UDSPath: w.vsockPath(b.ID)}).Exec(ctx, guestproto.Exec{Argv: []string{"sync"}, User: "root"}, io.Discard, io.Discard)
}

func (w *Worker) running(b *Box) *vm {
	return &vm{w: w, box: b, slot: slot(b.Slot), jail: w.jailRoot(b.ID), api: firecracker.NewClient(filepath.Join(w.jailRoot(b.ID), jailAPI))}
}

// snapshotGen pauses a running box and writes its next generation: the memory file
// is the previous one with the diff applied, and the disk is reflinked while the VM
// is paused, so both match. The VM stays paused. On failure (a full disk, say) what
// was written is dropped and the VM resumed. The caller saves the box, then calls
// cleanup to remove the previous generation.
func (w *Worker) snapshotGen(ctx context.Context, b *Box, m *vm) (cleanup func(), err error) {
	prev := b.Generation
	gen := w.genDir(b.ID, prev+1)
	w.syncGuest(ctx, b)
	state, diff, err := m.snapshot(ctx)
	fail := func(err error) (func(), error) {
		os.RemoveAll(gen)
		os.Remove(filepath.Join(m.jail, jailSnap, "vmstate"))
		os.Remove(filepath.Join(m.jail, jailSnap, "mem"))
		if rerr := m.api.Resume(ctx); rerr != nil {
			return nil, fmt.Errorf("%w; and resuming the VM failed: %v", err, rerr)
		}
		return nil, err
	}
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return fail(err)
	}
	mem := filepath.Join(gen, "mem")
	if b.BaseMem != "" {
		if err := clone(b.BaseMem, mem); err != nil {
			return fail(err)
		}
		if err := applyDiff(mem, diff); err != nil {
			return fail(err)
		}
		os.Remove(diff)
	} else {
		if err := os.Rename(diff, mem); err != nil {
			return fail(err)
		}
		if err := applyDiff(mem, mem); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(state, filepath.Join(gen, "vmstate")); err != nil {
		return fail(err)
	}
	if err := clone(filepath.Join(w.boxDir(b.ID), "rootfs.ext4"), filepath.Join(gen, "rootfs.ext4")); err != nil {
		return fail(err)
	}
	b.Generation, b.Checkpointed = prev+1, time.Now().UTC()
	return func() {
		if prev > 0 {
			os.RemoveAll(w.genDir(b.ID, prev))
		}
	}, nil
}

func (w *Worker) Remove(id string) error {
	unlock, err := w.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return err
	}
	w.killFirecracker(b)
	if b.Slot != 0 {
		slot(b.Slot).teardown()
	}
	if err := os.RemoveAll(w.boxDir(id)); err != nil {
		return err
	}
	os.Remove(filepath.Join(w.Root, "boxes", "."+id+".lock"))
	return nil
}

func (w *Worker) Exec(ctx context.Context, id string, e guestproto.Exec, stdout, stderr io.Writer) (int, error) {
	b, err := w.load(id)
	if err != nil {
		return 0, err
	}
	if b.State != Running {
		return 0, fmt.Errorf("box %q is %s", id, b.State)
	}
	c := &guestproto.Client{UDSPath: w.vsockPath(id)}
	return c.Exec(ctx, e, stdout, stderr)
}

// WriteFiles writes files into a running box and adds watch to the files its
// status reports hashes of.
func (w *Worker) WriteFiles(ctx context.Context, id string, f guestproto.Files, watch []string) error {
	b, err := w.load(id)
	if err != nil {
		return err
	}
	if b.State != Running {
		return fmt.Errorf("box %q is %s", id, b.State)
	}
	if len(f.Files) > 0 {
		if err := (&guestproto.Client{UDSPath: w.vsockPath(id)}).Files(ctx, f); err != nil {
			return err
		}
	}
	if len(watch) == 0 {
		return nil
	}
	unlock, err := w.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	if b, err = w.load(id); err != nil {
		return err
	}
	for _, p := range watch {
		if !slices.Contains(b.Watch, p) {
			b.Watch = append(b.Watch, p)
		}
	}
	return w.save(b)
}

// DialPort opens a byte stream to a TCP port on a running box's loopback.
func (w *Worker) DialPort(ctx context.Context, id string, port int) (net.Conn, error) {
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Running {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	return (&guestproto.Client{UDSPath: w.vsockPath(id)}).DialPort(ctx, port)
}

// DialGuest opens a raw connection to a running box's hutch-guest.
func (w *Worker) DialGuest(ctx context.Context, id string) (net.Conn, error) {
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Running {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	return (&guestproto.Client{UDSPath: w.vsockPath(id)}).Dial(ctx)
}

// CPUTime returns the total CPU time a running box's Firecracker has used.
func (w *Worker) CPUTime(b *Box) (time.Duration, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", b.PID))
	if err != nil {
		return 0, err
	}
	// Fields after the parenthesized command name; utime and stime are fields 14 and 15.
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 13 {
		return 0, fmt.Errorf("short /proc/%d/stat", b.PID)
	}
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	return time.Duration(utime+stime) * time.Second / 100, nil
}

func (w *Worker) GuestStatus(ctx context.Context, id string) (guestproto.Status, error) {
	b, err := w.load(id)
	if err != nil {
		return guestproto.Status{}, err
	}
	return (&guestproto.Client{UDSPath: w.vsockPath(id)}).Status(ctx, b.Watch...)
}

// Alive reports whether a running box's Firecracker process still exists.
func (w *Worker) Alive(b *Box) bool { return b.State == Running && b.PID > 0 && processAlive(b.PID) }

func (b *Box) ReachIP() string {
	if b.Slot == 0 {
		return ""
	}
	return slot(b.Slot).ReachIP()
}

// clone makes a copy-on-write copy (reflink) of src at dst.
func clone(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("reflink %s: %w (is %s on XFS with reflink?)", src, err, filepath.Dir(dst))
	}
	return out.Close()
}
