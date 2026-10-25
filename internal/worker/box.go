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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/firecracker"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// Boot creates a box from an image and cold boots it.
func (w *Worker) Boot(ctx context.Context, id, image string, vcpus, memMiB int) (*Box, error) {
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
	b := &Box{ID: id, Image: image, VCPUs: vcpus, MemMiB: memMiB, FCVersion: w.FCVersion, Class: w.Class(), Created: time.Now().UTC()}
	if err := clone(filepath.Join(w.imageDir(image), "rootfs.ext4"), filepath.Join(w.boxDir(id), "rootfs.ext4")); err != nil {
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

// Fork creates a box from a sleeping box's latest snapshot: the disk is reflinked and
// the memory file is shared copy-on-write until the new box writes to it.
func (w *Worker) Fork(ctx context.Context, srcID, id string) (*Box, error) {
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
	if src.Class != w.Class() {
		return nil, fmt.Errorf("box %q has snapshot class %s, this worker is %s", srcID, src.Class, w.Class())
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
	if b.State != Asleep {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	if b.Class != w.Class() {
		return nil, fmt.Errorf("box %q has snapshot class %s, this worker is %s; it needs a cold boot", id, b.Class, w.Class())
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
	s, err := w.newNetwork()
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
	b.State = Running
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

// Sleep snapshots a running box and stops its VM. The new memory file is the previous
// one with the diff applied; older generations are removed.
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
	m := &vm{w: w, box: b, slot: slot(b.Slot), jail: w.jailRoot(id), api: firecracker.NewClient(filepath.Join(w.jailRoot(id), jailAPI))}
	state, diff, err := m.snapshot(ctx)
	if err != nil {
		m.api.Resume(ctx)
		return nil, err
	}

	prev := b.Generation
	gen := w.genDir(id, prev+1)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return nil, err
	}
	mem := filepath.Join(gen, "mem")
	if b.BaseMem != "" {
		if err := clone(b.BaseMem, mem); err != nil {
			return nil, err
		}
		if err := applyDiff(mem, diff); err != nil {
			return nil, err
		}
	} else {
		if err := os.Rename(diff, mem); err != nil {
			return nil, err
		}
		if err := applyDiff(mem, mem); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(state, filepath.Join(gen, "vmstate")); err != nil {
		return nil, err
	}

	w.killFirecracker(b)
	slot(b.Slot).teardown()
	b.State, b.Slot, b.Generation, b.BaseMem = Asleep, 0, prev+1, ""
	if err := w.save(b); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(w.boxDir(id), "base.mem"))
	os.Remove(filepath.Join(w.boxDir(id), "base.vmstate"))
	if prev > 0 {
		os.RemoveAll(w.genDir(id, prev))
	}
	return b, nil
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
	return (&guestproto.Client{UDSPath: w.vsockPath(id)}).Status(ctx)
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
