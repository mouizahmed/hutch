//go:build windows

package main

import "os/exec"

func ownGroup(*exec.Cmd) {}

// signalGroup stops c: Windows has no gentler signal for another process.
func signalGroup(c *exec.Cmd, _ bool) {
	if c.Process != nil {
		c.Process.Kill()
	}
}
