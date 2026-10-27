package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The TUI is Hutch's own tmux server on this machine (-L hutch, its own config):
// the sidebar in the left pane, and the chosen thread's terminal in the right,
// switched with respawn-pane. Agents render natively; nothing is emulated.
const tuiSocket = "hutch"

func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", tuiSocket}, args...)...)
}

// runTUI opens the TUI, or reattaches it when it is already running.
func runTUI(cmd *cobra.Command, _ []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return cmd.Help()
	}
	if os.Getenv("HUTCH_TUI") != "" {
		return errors.New("already in the Hutch TUI")
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return errors.New("the TUI runs in tmux, which isn't installed; hutch ls and hutch attach work without it")
	}
	// Inside the user's own tmux too: Hutch's runs on its own socket.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") {
			env = append(env, e)
		}
	}
	if tmuxCmd("has-session", "-t", "hutch").Run() == nil {
		return syscall.Exec(tmux, []string{"tmux", "-L", tuiSocket, "attach-session", "-t", "hutch"}, env)
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	conf, err := writeTmuxConf()
	if err != nil {
		return err
	}
	return syscall.Exec(tmux, []string{"tmux", "-L", tuiSocket, "-f", conf, "new-session", "-s", "hutch", "-n", "hutch",
		"env HUTCH_TUI=1 " + shellQuote(exe) + " tui sidebar"}, env)
}

// selfPath is this binary as run, not resolved: package managers link it to
// versioned paths.
func selfPath() (string, error) {
	exe, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func shellJoin(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// Bindings have no prefix, so they work when hutch runs inside the user's own tmux.
// Ctrl-] moves between the sidebar and the right pane; Ctrl-\ reaches the sidebar
// as F12, which flips the right pane between agent and shell.
const tmuxConf = `set -g prefix None
unbind C-b
set -g mouse on
set -g escape-time 0
set -g focus-events on
set -g default-terminal tmux-256color
set -as terminal-features ",*:RGB"
set -g allow-passthrough on
set -g set-titles on
set -g set-titles-string hutch
set -g bell-action any
set -g visual-bell off
set -g pane-border-lines single
set -g pane-border-indicators off
set -g pane-border-style fg=colour240
set -g pane-active-border-style fg=colour240
set -g pane-border-status top
set -g pane-border-format "#[fg=colour250]#{?pane_title, #{pane_title} ,}"
set -g status on
set -g status-position bottom
set -g status-style bg=default,fg=colour244
set -g status-justify left
set -g status-left " Ctrl-] sidebar ↔ right · Ctrl-\\ agent ↔ shell · ? keys"
set -g status-left-length 120
set -g status-right ""
set -g window-status-format ""
set -g window-status-current-format ""
bind -n C-] select-pane -t :.+
bind -n 'C-\' send-keys -t '{left}' F12
`

func writeTmuxConf() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "hutch", "tmux.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(tmuxConf), 0o600)
}

func tuiCommand() *cobra.Command {
	c := &cobra.Command{Use: "tui", Hidden: true}
	c.AddCommand(&cobra.Command{
		Use:  "sidebar",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSidebar(cmd.Context()) },
	})
	c.AddCommand(&cobra.Command{
		Use:  "welcome",
		Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Print("\n  Choose a thread on the left and press Enter,\n  or n for a new task in a new box.\n\n  Ctrl-] moves between the sidebar and this pane.\n")
			select {}
		},
	})
	// connect shows one thread, waking its box, and reconnects on Enter after it
	// drops, so the right pane stays usable.
	c.AddCommand(&cobra.Command{
		Use:  "connect <box>/<thread> | <box> --shell",
		Args: cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			box, n, err := parseTarget(args[0])
			if err != nil {
				return err
			}
			in := bufio.NewReader(os.Stdin)
			for {
				b, err := findBox(ctx, sess, box)
				if err != nil {
					return err
				}
				if b.State == "asleep" || b.State == "archived" {
					fmt.Printf("waking %s…\n", box)
					if err := rpc.Call(ctx, sess, api.BoxWake, api.IDParams{ID: box}, nil); err != nil {
						fmt.Printf("%v\n", err)
					}
				}
				dir := b.Path
				if dir == "" {
					dir = "/workspace"
				}
				argv := tmuxAttach("shell-1", dir)
				if n > 0 {
					argv = viewArgv(n, true)
				}
				err = terminal(ctx, sess, box, argv)
				fmt.Print("\033[2J\033[H")
				if err != nil {
					fmt.Printf("%s: %v\n", args[0], err)
				}
				fmt.Printf("Disconnected from %s. Enter reconnects.\n", args[0])
				if _, err := in.ReadString('\n'); err != nil {
					return nil
				}
			}
		}),
	})
	return c
}

// viewArgv attaches a thread in a session of its own grouped with the box's
// "hutch" session, so several viewers can each show a different thread. Plain
// hides the inner tmux's status bar and prefix, for the TUI.
func viewArgv(n int, plain bool) []string {
	b := make([]byte, 4)
	rand.Read(b)
	name := "view-" + hex.EncodeToString(b)
	argv := []string{"tmux", "new-session", "-t", agentSession, "-s", name,
		";", "set-option", "destroy-unattached", "on", ";", "select-window", "-t", name + ":" + strconv.Itoa(n)}
	if plain {
		argv = append(argv, ";", "set-option", "status", "off", ";", "set-option", "prefix", "None")
	}
	return argv
}
