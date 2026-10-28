//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/firecracker"
)

// Paths inside every jail. Snapshots record these, so any box can restore any snapshot.
const (
	jailKernel = "/vmlinux"
	jailRootfs = "/rootfs.ext4"
	jailVsock  = "/v.sock"
	jailAPI    = "/run/firecracker.socket"
	jailState  = "/vmstate"
	jailMem    = "/mem"
	jailSnap   = "/snapshot"
	jailLog    = "/firecracker.log"
)

// vmmOverheadMiB is what a Firecracker process needs beyond its guest's memory.
const vmmOverheadMiB = 256

type vm struct {
	w    *Worker
	box  *Box
	slot slot
	jail string
	api  *firecracker.Client
}

func (w *Worker) vsockPath(id string) string   { return filepath.Join(w.jailRoot(id), jailVsock) }
func (w *Worker) jailAPIPath(id string) string { return filepath.Join(w.jailRoot(id), jailAPI) }

// startJailer prepares the chroot with hard links to the box's files and starts
// Firecracker in it, detached. The serial console goes to console.log and reads
// input from the console.in FIFO; Firecracker's own log goes to firecracker.log.
func (w *Worker) startJailer(ctx context.Context, b *Box, s slot, links map[string]string) (*vm, error) {
	jail := w.jailRoot(b.ID)
	w.saveFCLog(b.ID)
	os.RemoveAll(filepath.Dir(jail))
	uid := s.uid()
	for _, d := range []string{jail, filepath.Join(jail, "run"), filepath.Join(jail, jailSnap)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
		if err := os.Chown(d, uid, uid); err != nil {
			return nil, err
		}
	}
	for inJail, src := range links {
		if err := os.Link(src, filepath.Join(jail, inJail)); err != nil {
			return nil, fmt.Errorf("link %s into jail: %w", src, err)
		}
	}
	if err := os.Chown(filepath.Join(w.boxDir(b.ID), "rootfs.ext4"), uid, uid); err != nil {
		return nil, err
	}
	fcLog, err := os.OpenFile(filepath.Join(jail, jailLog), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fcLog.Close()
	if err := os.Chown(filepath.Join(jail, jailLog), uid, uid); err != nil {
		return nil, err
	}

	// Opened read-write so Firecracker never sees end of file between writers.
	in := w.consoleInPath(b.ID)
	os.Remove(in)
	if err := unix.Mkfifo(in, 0o600); err != nil {
		return nil, err
	}
	stdin, err := os.OpenFile(in, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer stdin.Close()

	console, err := os.OpenFile(w.ConsolePath(b.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer console.Close()
	fmt.Fprintf(console, "\n--- hutchd: starting %s at %s ---\n", b.ID, time.Now().UTC().Format(time.RFC3339))

	cmd := exec.Command(w.fcBin(b.FCVersion, "jailer"),
		"--id", b.ID,
		"--exec-file", w.fcBin(b.FCVersion, "firecracker"),
		"--uid", fmt.Sprint(uid), "--gid", fmt.Sprint(uid),
		"--chroot-base-dir", filepath.Join(w.Root, "jail"),
		"--netns", s.netnsPath(),
		"--cgroup-version", "2",
		// A VM holds at most its memory plus the VMM's own: a runaway one is
		// throttled, not allowed to take the worker's memory.
		"--cgroup", fmt.Sprintf("memory.high=%d", int64(b.MemMiB+vmmOverheadMiB)<<20),
		"--", "--api-sock", jailAPI, "--log-path", jailLog)
	cmd.Stdin = stdin
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.PID = cmd.Process.Pid
	go cmd.Wait()

	m := &vm{w: w, box: b, slot: s, jail: jail, api: firecracker.NewClient(filepath.Join(jail, jailAPI))}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := firecracker.WaitForSocket(waitCtx, filepath.Join(jail, jailAPI)); err != nil {
		m.kill()
		return nil, err
	}
	return m, nil
}

func (m *vm) coldBoot(ctx context.Context, vendor string) error {
	tmpl, err := firecracker.CPUTemplate(vendor)
	if err != nil {
		return err
	}
	steps := []func() error{
		func() error {
			return m.api.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUs: m.box.VCPUs, MemMiB: m.box.MemMiB, TrackDirtyPages: true})
		},
		func() error { return m.api.PutCPUConfig(ctx, tmpl) },
		func() error {
			return m.api.PutBootSource(ctx, firecracker.BootSource{
				KernelImagePath: jailKernel,
				BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off clocksource=kvm-clock init=/usr/local/bin/hutch-guest " + bootIPArg,
			})
		},
		func() error {
			return m.api.PutDrive(ctx, firecracker.Drive{ID: "rootfs", PathOnHost: jailRootfs, IsRootDevice: true, IOEngine: "Async"})
		},
		func() error {
			return m.api.PutNetworkInterface(ctx, firecracker.NetworkInterface{ID: "eth0", HostDevName: "tap0", GuestMAC: guestMAC})
		},
		func() error { return m.api.PutVsock(ctx, firecracker.Vsock{GuestCID: 3, UDSPath: jailVsock}) },
		func() error {
			return m.api.PutBalloon(ctx, firecracker.Balloon{DeflateOnOOM: true, StatsPollingSecs: 1, FreePageReporting: true})
		},
		func() error { return m.api.Start(ctx) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (m *vm) restore(ctx context.Context) error {
	return m.api.LoadSnapshot(ctx, jailState, jailMem)
}

// snapshot pauses the VM and writes a diff snapshot into the jail, returning host paths.
func (m *vm) snapshot(ctx context.Context) (state, mem string, err error) {
	if err := m.api.Pause(ctx); err != nil {
		return "", "", err
	}
	if err := m.api.CreateDiffSnapshot(ctx, jailSnap+"/vmstate", jailSnap+"/mem"); err != nil {
		return "", "", err
	}
	return filepath.Join(m.jail, jailSnap, "vmstate"), filepath.Join(m.jail, jailSnap, "mem"), nil
}

func (m *vm) kill() { m.w.killFirecracker(m.box) }

// killFirecracker stops the box's Firecracker and removes its jail and cgroup.
func (w *Worker) killFirecracker(b *Box) {
	if b.PID > 0 && processAlive(b.PID) {
		syscall.Kill(b.PID, syscall.SIGKILL)
		for i := 0; i < 500 && processAlive(b.PID); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	b.PID = 0
	w.saveFCLog(b.ID)
	os.RemoveAll(filepath.Dir(w.jailRoot(b.ID)))
	os.Remove(w.consoleInPath(b.ID))
	os.Remove(filepath.Join("/sys/fs/cgroup/firecracker", b.ID))
}

// saveFCLog appends the log of the box's last Firecracker run, which lives in the
// jail, to firecracker.log in the box directory.
func (w *Worker) saveFCLog(id string) {
	src, err := os.Open(filepath.Join(w.jailRoot(id), jailLog))
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(w.FCLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer dst.Close()
	io.Copy(dst, src)
}

// ConsoleInput opens a running box's serial console input.
func (w *Worker) ConsoleInput(id string) (*os.File, error) {
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if !w.Alive(b) {
		return nil, fmt.Errorf("box %q is not running", id)
	}
	f, err := os.OpenFile(w.consoleInPath(id), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENXIO) {
		return nil, fmt.Errorf("box %q was started without console input; sleep and wake it first", id)
	}
	return f, err
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
