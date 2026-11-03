//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// ownGroup starts c in a process group of its own, so stopping it stops what it
// started too.
func ownGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// signalGroup asks c's group to stop, or with kill, stops it.
func signalGroup(c *exec.Cmd, kill bool) {
	if c.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-c.Process.Pid, sig)
}
