//go:build linux

package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// watchPressure runs a check as soon as the kernel reports tasks stalling on
// memory for worker.pressure_stall within a second: the worker is about to run
// out, sooner than the next check would notice. It re-arms when the setting changes.
func (m *monitor) watchPressure(ctx context.Context) {
	for ctx.Err() == nil {
		stall := m.settings.Load().PressureStall
		if stall == 0 {
			stall = 50 * time.Millisecond
		}
		if err := m.armPressure(ctx, stall); err != nil {
			log.Printf("memory pressure events unavailable: %v", err)
			return
		}
	}
}

// armPressure watches with one threshold until ctx ends or the setting changes.
func (m *monitor) armPressure(ctx context.Context, stall time.Duration) error {
	f, err := os.OpenFile("/proc/pressure/memory", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "some %d 1000000", stall.Microseconds()); err != nil {
		return err
	}
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLPRI}}
	for ctx.Err() == nil && m.settings.Load().PressureStall == stall {
		n, err := unix.Poll(fds, 1000)
		if err != nil && err != unix.EINTR {
			return err
		}
		if n > 0 && fds[0].Revents&unix.POLLERR != 0 {
			return fmt.Errorf("the kernel stopped the trigger")
		}
		if n > 0 && fds[0].Revents&unix.POLLPRI != 0 {
			log.Printf("memory: pressure event from the kernel")
			m.kick()
		}
	}
	return nil
}

// pressureEvents reports whether the kernel can signal memory pressure.
func pressureEvents() error {
	f, err := os.OpenFile("/proc/pressure/memory", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("some 50000 1000000")
	return err
}
