package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/charmbracelet/huh"
	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The TUI is Hutch's own tmux server on this machine (-L hutch, its own config):
// the sidebar in the left pane, and the chosen thread's terminal in the right,
// switched with respawn-pane. Agents render natively; nothing is emulated.
const tuiSocket = "hutch"

func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", tuiSocket}, args...)...)
}

// runTUI opens the TUI, one tab per Hutch project, on the current folder's project
// when it is one, reattaching when the TUI is already running. Each tab is a tmux
// window: its own sidebar and right pane.
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
	sess, err := connect()
	if err != nil {
		return err
	}
	defer sess.Close()
	var projects []api.Project
	if err := rpc.Call(cmd.Context(), sess, api.ProjectList, struct{}{}, &projects); err != nil {
		return err
	}
	boxes, err := listBoxes(cmd.Context(), sess)
	if err != nil {
		return err
	}
	current := ""
	if repo, _ := config.FindRepo("."); repo != nil {
		for _, p := range projects {
			if p.ID == repo.ID {
				current = p.ID
			}
		}
	}
	attach := func() error {
		return syscall.Exec(tmux, []string{"tmux", "-L", tuiSocket, "attach-session", "-t", "hutch"}, env)
	}
	if tmuxCmd("has-session", "-t", "hutch").Run() == nil {
		if current != "" {
			if err := openTab(current, projects, boxes); err != nil {
				return err
			}
		}
		return attach()
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	conf, err := writeTmuxConf(exe)
	if err != nil {
		return err
	}
	// The current project first, then the rest by name; with none, one tab for
	// boxes outside projects.
	sort.SliceStable(projects, func(i, j int) bool {
		if (projects[i].ID == current) != (projects[j].ID == current) {
			return projects[i].ID == current
		}
		return projects[i].Slug < projects[j].Slug
	})
	if len(projects) == 0 {
		projects = []api.Project{{Slug: "hutch"}}
	}
	for i, p := range projects {
		args := append([]string{"new-window", "-d", "-t", "hutch:"}, tabArgs(exe, p, boxes)...)
		if i == 0 {
			args = append([]string{"-f", conf, "new-session", "-d", "-s", "hutch"}, tabArgs(exe, p, boxes)...)
		}
		if out, err := tmuxCmd(args...).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux: %v: %s", err, out)
		}
	}
	return attach()
}

// tabArgs are a project tab's window name, folder, and sidebar command.
func tabArgs(exe string, p api.Project, boxes []api.Box) []string {
	dir := projectDir(p.ID, boxes)
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	return []string{"-n", p.Slug, "-c", dir, "env HUTCH_TUI=1 " + shellJoin(exe, "tui", "sidebar", "--project", p.ID)}
}

// openTab selects a project's tab, opening one if there is none.
func openTab(project string, projects []api.Project, boxes []api.Box) error {
	out, err := tmuxCmd("list-windows", "-t", "hutch", "-F", "#{window_id} #{@hutch-key}").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id, k, ok := strings.Cut(line, " "); ok && k == project {
			return tmuxCmd("select-window", "-t", id).Run()
		}
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	for _, p := range projects {
		if p.ID == project {
			return tmuxCmd(append([]string{"new-window", "-t", "hutch:"}, tabArgs(exe, p, boxes)...)...).Run()
		}
	}
	return fmt.Errorf("no project %s", project)
}

// projectDir is where a project is checked out on this machine: where hutch last
// ran in it, else where its boxes came from, if that still exists.
func projectDir(project string, boxes []api.Box) string {
	if d := projectPaths()[project]; d != "" {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	for _, b := range boxes {
		if b.Project == project && b.Path != "" {
			if _, err := os.Stat(b.Path); err == nil {
				return b.Path
			}
		}
	}
	return ""
}

func projectPathsFile() string {
	dir, _ := os.UserCacheDir()
	return filepath.Join(dir, "hutch", "projects.json")
}

func projectPaths() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(projectPathsFile()); err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

// rememberProject records where a repository is checked out, for its tab.
func rememberProject(id, root string) {
	m := projectPaths()
	if m[id] == root {
		return
	}
	m[id] = root
	data, _ := json.Marshal(m)
	os.MkdirAll(filepath.Dir(projectPathsFile()), 0o700)
	os.WriteFile(projectPathsFile(), data, 0o600)
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
// as F12, which flips the right pane between agent and shell. The status line at
// the top is the project tabs: + opens one, × closes one, each sidebar sets its
// tab's counts in @hutch-status.
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
set -g automatic-rename off
set -g allow-rename off
set -g renumber-windows on
set -g bell-action any
set -g visual-bell off
set -g pane-border-lines single
set -g pane-border-indicators off
set -g pane-border-style fg=colour240
set -g pane-active-border-style fg=colour240
set -g pane-border-status top
set -g pane-border-format "#[fg=colour250]#{?pane_title, #{pane_title} ,}"
set -g status on
set -g status-position top
set -g status-style bg=default,fg=colour244
set -g status-justify left
set -g status-left "#[fg=colour250] + "
set -g status-left-length 6
set -g window-status-separator "  "
set -g window-status-style default
set -g window-status-current-style default
set -g window-status-last-style default
set -g window-status-activity-style default
set -g window-status-bell-style default
set -g window-status-format "#[fg=colour250] #W#{?@hutch-status, #{E:@hutch-status},} #[range=user|close#{window_id},fg=colour240]×#[norange] "
set -g window-status-current-format "#[bg=colour238,fg=colour255,bold] #W#[nobold]#{?@hutch-status, #{E:@hutch-status},} #[range=user|close#{window_id},fg=colour244]×#[norange] #[bg=default]"
set -g status-right "#[fg=colour244]Ctrl-] sidebar ↔ right · Ctrl-\\ agent ↔ shell · ? keys "
set -g status-right-length 80
set-hook -g window-resized "resize-pane -t '{left}' -x SIDEBAR"
set-hook -g client-resized "resize-pane -t '{left}' -x SIDEBAR"
bind -n C-] select-pane -t :.+
bind -n 'C-\' send-keys -t '{left}' F12
bind -n C-PPage previous-window
bind -n C-NPage next-window
bind -n MouseDown1Status if -F "#{m:close*,#{mouse_status_range}}" "run-shell \"tmux kill-window -t '#{s/close//:mouse_status_range}'\"" "select-window -t ="
bind -n MouseDown1StatusLeft display-popup -E -w 70 -h 16 -T " Open a project " EXE tui add
`

func writeTmuxConf(exe string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "hutch", "tmux.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	conf := strings.ReplaceAll(tmuxConf, "SIDEBAR", strconv.Itoa(sidebarWidth))
	conf = strings.ReplaceAll(conf, "EXE", exe)
	return path, os.WriteFile(path, []byte(conf), 0o600)
}

func tuiCommand() *cobra.Command {
	c := &cobra.Command{Use: "tui", Hidden: true}
	var project string
	sidebarCmd := &cobra.Command{
		Use:  "sidebar",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSidebar(cmd.Context(), project) },
	}
	sidebarCmd.Flags().StringVar(&project, "project", "", "the tab's project")
	c.AddCommand(sidebarCmd)
	c.AddCommand(&cobra.Command{
		Use:  "welcome",
		Args: cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Print("\n  Choose a thread on the left and press Enter,\n  or n for a new task in a new box.\n\n  Ctrl-] moves between the sidebar and this pane.\n")
			select {}
		},
	})
	// add opens the tab of a project that has none.
	c.AddCommand(&cobra.Command{
		Use:  "add",
		Args: cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			var projects []api.Project
			if err := rpc.Call(ctx, sess, api.ProjectList, struct{}{}, &projects); err != nil {
				return err
			}
			boxes, err := listBoxes(ctx, sess)
			if err != nil {
				return err
			}
			out, _ := tmuxCmd("list-windows", "-t", "hutch", "-F", "#{@hutch-key}").Output()
			open := map[string]bool{}
			for _, k := range strings.Split(string(out), "\n") {
				open[strings.TrimSpace(k)] = true
			}
			var opts []huh.Option[string]
			for _, p := range projects {
				if !open[p.ID] {
					opts = append(opts, huh.NewOption(p.Slug+"  "+p.ID, p.ID))
				}
			}
			if len(opts) == 0 {
				fmt.Print("\n  Every project has a tab. A new one starts with hutch init in its repository.\n\n  Press Enter.")
				fmt.Scanln()
				return nil
			}
			var id string
			if err := huh.NewSelect[string]().Title("Open which project?").Options(opts...).Value(&id).Run(); err != nil {
				return nil
			}
			return openTab(id, projects, boxes)
		}),
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
