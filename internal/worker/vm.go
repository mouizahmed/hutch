//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

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
)

type vm struct {
	w    *Worker
	box  *Box
	slot slot
	jail string
	api  *firecracker.Client
}

func (w *Worker) vsockPath(id string) string { return filepath.Join(w.jailRoot(id), jailVsock) }

// startJailer prepares the chroot with hard links to the box's files and starts
// Firecracker in it, detached, with the serial console going to console.log.
func (w *Worker) startJailer(ctx context.Context, b *Box, s slot, links map[string]string) (*vm, error) {
	jail := w.jailRoot(b.ID)
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

	console, err := os.OpenFile(w.ConsolePath(b.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer console.Close()
	fmt.Fprintf(console, "\n--- hutchd: starting %s at %s ---\n", b.ID, time.Now().UTC().Format(time.RFC3339))

	cmd := exec.Command(w.fcBin("jailer"),
		"--id", b.ID,
		"--exec-file", w.fcBin("firecracker"),
		"--uid", fmt.Sprint(uid), "--gid", fmt.Sprint(uid),
		"--chroot-base-dir", filepath.Join(w.Root, "jail"),
		"--netns", s.netnsPath(),
		"--cgroup-version", "2",
		"--", "--api-sock", jailAPI)
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
				BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off clocksource=kvm-clock " + bootIPArg,
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
	os.RemoveAll(filepath.Dir(w.jailRoot(b.ID)))
	os.Remove(filepath.Join("/sys/fs/cgroup/firecracker", b.ID))
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
