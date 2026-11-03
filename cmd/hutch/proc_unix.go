//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// procGroup is a started command and what it starts: a process group of its own.
type procGroup struct{ pid int }

func startGroup(c *exec.Cmd) (*procGroup, error) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &procGroup{c.Process.Pid}, nil
}

// signal asks the group to stop, or with kill, stops it.
func (g *procGroup) signal(kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-g.pid, sig)
}

// alive reports whether anything in the group still runs.
func (g *procGroup) alive() bool { return syscall.Kill(-g.pid, 0) == nil }
