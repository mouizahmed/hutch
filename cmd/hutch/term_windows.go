//go:build windows

package main

import (
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// resized signals each change of the terminal's size until stop. Windows has no
// SIGWINCH, so the size is polled.
func resized() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		fd := int(os.Stdout.Fd())
		w, h, _ := term.GetSize(fd)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if nw, nh, err := term.GetSize(fd); err == nil && (nw != w || nh != h) {
					w, h = nw, nh
					select {
					case ch <- os.Interrupt:
					default:
					}
				}
			}
		}
	}()
	return ch, func() { close(done) }
}

// prepareTerminal turns on the console's handling of escape sequences, which a
// box's terminal output is made of; Windows Terminal has it on already.
func prepareTerminal() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
}

// editorCommand opens file in editor, a command line for cmd.exe.
func editorCommand(editor, file string) *exec.Cmd {
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{CmdLine: `cmd.exe /c ` + editor + ` "` + file + `"`}
	return cmd
}

const defaultEditor = "notepad"

// laptopPath is where a laptop directory appears in a box: a Windows path has no
// place in Linux, so boxes work in /workspace.
func laptopPath(string) string { return "" }

// nullFile is the system's empty file, for ssh's config.
const nullFile = "NUL"

// movesSessions: hutch cloud and local carry agent sessions, whose paths are the
// laptop's.
const movesSessions = false
