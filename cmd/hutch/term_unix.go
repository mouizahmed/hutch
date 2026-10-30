//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// resized signals each change of the terminal's size until stop.
func resized() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}

// prepareTerminal readies the terminal for a box's output.
func prepareTerminal() {}

// editorCommand opens file in editor, a shell command line.
func editorCommand(editor, file string) *exec.Cmd {
	return exec.Command("sh", "-c", editor+` "$1"`, "sh", file)
}

const defaultEditor = "vi"

// laptopPath is where a laptop directory appears in a box: the same path.
func laptopPath(dir string) string { return dir }

// nullFile is the system's empty file, for ssh's config.
const nullFile = "/dev/null"

// movesSessions: hutch cloud and local carry agent sessions, whose paths are the
// laptop's.
const movesSessions = true
