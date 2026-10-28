package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// withServer runs f with a session to the server.
func withServer(f func(ctx context.Context, sess *yamux.Session, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		sess, err := connect()
		if err != nil {
			return err
		}
		defer sess.Close()
		return f(cmd.Context(), sess, args)
	}
}

func listBoxes(ctx context.Context, sess *yamux.Session) ([]api.Box, error) {
	var boxes []api.Box
	return boxes, rpc.Call(ctx, sess, api.BoxList, struct{}{}, &boxes)
}

func boxCommands() []*cobra.Command {
	var name, from, image string
	var cpus, memMiB int
	var dryRun bool
	var agent agentFlags
	newCmd := &cobra.Command{
		Use:     `new ["<prompt>"]`,
		Short:   "Create a box and start an agent in it",
		GroupID: "boxes",
		Args:    cobra.MaximumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			var err error
			if name == "" {
				if name, err = newBoxName(ctx, sess); err != nil {
					return err
				}
			}
			cfg, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			if cfg, err = applyGrants(ctx, sess, cfg); err != nil {
				return err
			}
			ensureGitIdentity(ctx, sess)
			if cpus == 0 {
				cpus = int(cfg.Int("size.cpu"))
			}
			if memMiB == 0 {
				memMiB = int(cfg.Size("size.memory") >> 20)
			}
			idle := api.Idle{Timeout: cfg.Duration("idle_timeout"), CPUThreshold: cfg.Percent("idle.cpu_threshold")}
			start := time.Now()
			var b api.Box
			repo, err := config.FindRepo(".")
			if err != nil {
				return err
			}
			if image != "" {
				p := api.BootParams{ID: name, Image: image, VCPUs: cpus, MemMiB: memMiB, DiskBytes: cfg.Size("disk.size"), Idle: idle}
				if err := rpc.Call(ctx, sess, api.BoxBoot, p, &b); err != nil {
					return fmt.Errorf("%w\n  hutch console %s", err, name)
				}
			} else if from == "" && repo != nil {
				p := api.ForkParams{Project: repo.ID, Path: repo.Root, Base: cfg.String("base"), ID: name, Idle: idle, Credentials: cfg.List("credentials"),
					PushInterval: cfg.Duration("git.push_interval"), Allow: cfg.List("network.allow")}
				if err := checkAllow(p.Allow); err != nil {
					return err
				}
				if dryRun {
					return printPlan(ctx, sess, cfg, p, agent)
				}
				if err := rpc.CallOutput(ctx, sess, api.BoxFork, p, &b, os.Stdout, os.Stderr); err != nil {
					return err
				}
			} else {
				if from == "" {
					from = "base"
				}
				if err := rpc.Call(ctx, sess, api.BoxFork, api.ForkParams{Source: from, ID: name, Idle: idle, Credentials: cfg.List("credentials")}, &b); err != nil {
					return err
				}
			}
			t, err := startThread(ctx, sess, cfg, b.ID, agent, args)
			if err != nil {
				return fmt.Errorf("box %s created, but the thread failed: %w", b.ID, err)
			}
			fmt.Printf("%s/%d %s started in %s\n", b.ID, t.N, t.Agent, time.Since(start).Round(time.Millisecond))
			fmt.Printf("  hutch attach %s/%d\n", b.ID, t.N)
			return nil
		}),
	}
	agent.register(newCmd)
	newCmd.Flags().StringVar(&name, "name", "", "box name (default: generated, like quiet-fox)")
	newCmd.Flags().StringVar(&from, "from", "", "sleeping box to fork from (default: this project's template, else base)")
	newCmd.Flags().StringVar(&image, "image", "", "cold boot from this image on the machine instead of forking")
	newCmd.Flags().IntVar(&cpus, "cpus", 0, "vCPUs (with --image; default: size.cpu)")
	newCmd.Flags().IntVar(&memMiB, "mem", 0, "memory in MiB (with --image; default: size.memory)")
	newCmd.MarkFlagsMutuallyExclusive("from", "image")
	newCmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen (template, logins, network) and start nothing")

	ls := &cobra.Command{
		Use:     "ls",
		Short:   "List boxes",
		GroupID: "boxes",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			boxes, err := listBoxes(ctx, sess)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "BOX / THREAD\tAGENT\tSTATE\tSIZE\tCREATED")
			for _, b := range boxes {
				if b.Kind == api.BoxVersion {
					continue
				}
				state := b.State
				if b.Merged != "" {
					state += " ✓ merged"
				}
				fmt.Fprintf(tw, "%s\t\t%s\t%d vCPU, %s\t%s\n", b.ID, state, b.VCPUs, mib(b.MemMiB), ago(b.Created))
				if b.PushError != "" {
					msg, _, _ := strings.Cut(b.PushError, "\n")
					fmt.Fprintf(tw, "  ! push of %s failed: %s\t\t\t\t\n", b.Branch, msg)
				}
				for _, t := range b.Threads {
					pin := ""
					if t.Pinned {
						pin = "* "
					}
					fmt.Fprintf(tw, "  %d  %s%s\t%s\t%s\t\t%s\n", t.N, pin, t.Title, t.Agent, t.State, ago(t.Created))
				}
			}
			return tw.Flush()
		}),
	}

	var user string
	execCmd := &cobra.Command{
		Use:     "exec <box> -- <command> [args...]",
		Short:   "Run one command in a box; output and exit code",
		GroupID: "boxes",
		Args:    cobra.MinimumNArgs(2),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: args[0], Argv: args[1:], User: user}, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError(code)
			}
			return nil
		}),
	}
	execCmd.Flags().StringVar(&user, "user", "dev", "user to run as")

	var follow, interactive bool
	console := &cobra.Command{
		Use:     "console <box>",
		Short:   "Show a box's serial console (works when the guest is broken)",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			if interactive {
				return serialConsole(ctx, sess, args[0])
			}
			_, err := rpc.Exec(ctx, sess, api.BoxConsole, api.ConsoleParams{ID: args[0], Follow: follow}, os.Stdout, os.Stderr)
			if ctx.Err() != nil {
				return nil
			}
			return err
		}),
	}
	console.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output")
	console.Flags().BoolVarP(&interactive, "interactive", "i", false, "type into the console of a running box (root shell)")
	console.MarkFlagsMutuallyExclusive("follow", "interactive")

	state := func(use, short, method string) *cobra.Command {
		return &cobra.Command{
			Use:     use + " <box>",
			Short:   short,
			GroupID: "boxes",
			Args:    cobra.ExactArgs(1),
			RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
				start := time.Now()
				var b api.Box
				if err := rpc.Call(ctx, sess, method, api.IDParams{ID: args[0]}, &b); err != nil {
					return err
				}
				fmt.Printf("%s %s in %s\n", b.ID, b.State, time.Since(start).Round(time.Millisecond))
				return nil
			}),
		}
	}

	rm := &cobra.Command{
		Use:     "rm <box>",
		Short:   "Delete a box and its snapshots",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := findBox(ctx, sess, args[0])
			if err != nil {
				return err
			}
			if err := rpc.Call(ctx, sess, api.BoxRm, api.IDParams{ID: args[0]}, nil); err != nil {
				return err
			}
			fmt.Println(args[0], "removed")
			return cleanUpGit(b)
		}),
	}

	return []*cobra.Command{
		newCmd, ls, execCmd, console,
		state("sleep", "Snapshot a box and stop its VM", api.BoxSleep),
		state("wake", "Restore a sleeping box", api.BoxWake),
		rm,
	}
}

func mib(m int) string {
	if m%1024 == 0 {
		return fmt.Sprintf("%d GB", m/1024)
	}
	return fmt.Sprintf("%d MiB", m)
}

type agentFlags struct {
	agent, cmd string
}

func (a *agentFlags) register(c *cobra.Command) {
	c.Flags().StringVar(&a.agent, "agent", "", "agent to run: "+strings.Join(agents.Builtin().Names(), ", ")+" (default: default_agent)")
	c.Flags().StringVar(&a.cmd, "cmd", "", "run any terminal command instead of an agent")
	c.MarkFlagsMutuallyExclusive("agent", "cmd")
}

func startThread(ctx context.Context, sess *yamux.Session, cfg *config.Config, box string, a agentFlags, args []string) (api.Thread, error) {
	p := api.ThreadParams{Box: box, Agent: a.agent, Cmd: a.cmd, Autonomy: cfg.String("autonomy")}
	if len(args) > 0 {
		p.Prompt = args[0]
	}
	if p.Agent == "" && p.Cmd == "" {
		p.Agent = cfg.String("default_agent")
	}
	var t api.Thread
	return t, rpc.Call(ctx, sess, api.ThreadNew, p, &t)
}

// parseTarget splits "box" or "box/3".
func parseTarget(s string) (box string, n int, err error) {
	box, num, ok := strings.Cut(s, "/")
	if !ok {
		return box, 0, nil
	}
	n, err = strconv.Atoi(num)
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("invalid thread %q: want <box>/<number>", s)
	}
	return box, n, nil
}

func threadCommands() []*cobra.Command {
	var agent agentFlags
	thread := &cobra.Command{
		Use:     `thread <box> ["<prompt>"]`,
		Short:   "Start another agent thread in a box",
		GroupID: "boxes",
		Args:    cobra.RangeArgs(1, 2),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			cfg, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			if args[0] == "template" {
				return templateThread(ctx, sess, cfg, agent, args[1:])
			}
			t, err := startThread(ctx, sess, cfg, args[0], agent, args[1:])
			if err != nil {
				return err
			}
			fmt.Printf("%s/%d %s started\n  hutch attach %s/%d\n", t.Box, t.N, t.Agent, t.Box, t.N)
			return nil
		}),
	}
	agent.register(thread)
	thread.AddCommand(threadSubcommands()...)

	send := &cobra.Command{
		Use:     `send <box>/<thread> "<message>"`,
		Short:   "Type a message into a thread",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(2),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			box, n, err := parseTarget(args[0])
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("which thread? use %s/<number>", box)
			}
			return rpc.Call(ctx, sess, api.ThreadSend, api.SendParams{Box: box, N: n, Message: args[1]}, nil)
		}),
	}
	return []*cobra.Command{thread, send}
}

func threadSubcommands() []*cobra.Command {
	target := func(use, short string, run func(ctx context.Context, sess *yamux.Session, box string, n int, args []string) error) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.MinimumNArgs(1),
			RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
				box, n, err := parseTarget(args[0])
				if err != nil {
					return err
				}
				if n == 0 {
					return fmt.Errorf("which thread? use %s/<number>", box)
				}
				return run(ctx, sess, box, n, args[1:])
			}),
		}
	}
	rm := target("rm <box>/<thread>", "Stop a thread and forget it", func(ctx context.Context, sess *yamux.Session, box string, n int, _ []string) error {
		return rpc.Call(ctx, sess, api.ThreadRm, api.ThreadTarget{Box: box, N: n}, nil)
	})
	pin := target("pin <box>/<thread>", "Pin a thread to the top of lists", func(ctx context.Context, sess *yamux.Session, box string, n int, _ []string) error {
		return rpc.Call(ctx, sess, api.ThreadPin, api.ThreadTarget{Box: box, N: n, Pinned: true}, nil)
	})
	unpin := target("unpin <box>/<thread>", "Unpin a thread", func(ctx context.Context, sess *yamux.Session, box string, n int, _ []string) error {
		return rpc.Call(ctx, sess, api.ThreadPin, api.ThreadTarget{Box: box, N: n}, nil)
	})
	rename := target(`rename <box>/<thread> "<title>"`, "Give a thread your own title", func(ctx context.Context, sess *yamux.Session, box string, n int, args []string) error {
		if len(args) != 1 {
			return errors.New("give one title")
		}
		return rpc.Call(ctx, sess, api.ThreadRename, api.ThreadTarget{Box: box, N: n, Title: args[0]}, nil)
	})
	resume := target("resume <box>/<thread>", "Restart an exited agent in its own session", func(ctx context.Context, sess *yamux.Session, box string, n int, _ []string) error {
		var t api.Thread
		if err := rpc.Call(ctx, sess, api.ThreadResume, api.ThreadTarget{Box: box, N: n}, &t); err != nil {
			return err
		}
		fmt.Printf("%s/%d resumed %s session %s\n  hutch attach %s/%d\n", box, n, t.Agent, t.SessionID, box, n)
		return nil
	})
	return []*cobra.Command{rm, pin, unpin, rename, resume}
}

func statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "status <box>",
		Short:   "Show a box and its threads, with token counts",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			b, err := findBox(ctx, sess, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s, %d vCPU, %s, on %s\n", b.ID, b.State, b.VCPUs, mib(b.MemMiB), b.Worker)
			if b.Branch != "" {
				fmt.Printf("branch %s (base %s)\n", b.Branch, b.Base)
			}
			if b.PushError != "" {
				fmt.Printf("push failed: %s\n", b.PushError)
			}
			if len(b.Threads) == 0 {
				return nil
			}
			fmt.Println()
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "THREAD\tAGENT\tSTATE\tTOKENS\tSESSION")
			for _, t := range b.Threads {
				tokens := ""
				if t.Tokens > 0 {
					tokens = fmt.Sprint(t.Tokens)
				}
				fmt.Fprintf(tw, "%d  %s\t%s\t%s\t%s\t%s\n", t.N, t.Title, t.Agent, t.State, tokens, t.SessionID)
			}
			return tw.Flush()
		}),
	}
}

// perHour shows a price per hour, or "-" for none (asleep, or a persistent worker).
func moveCommand() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:     "move <box>",
		Short:   "Move a box to another worker (warm when it can, else cold)",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var r api.MoveResult
			if err := rpc.CallOutput(cmd.Context(), sess, api.BoxMove, api.MoveParams{ID: args[0], To: to}, &r, os.Stdout, os.Stderr); err != nil {
				return err
			}
			switch r.Kind {
			case "warm":
				fmt.Printf("Warm move from %s to %s: everything kept running, agents mid-conversation.\n", r.From, r.To)
			default:
				fmt.Printf("Cold move from %s to %s (another snapshot class): files kept, processes restarted, agents resumed.\n", r.From, r.To)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "worker to move to (default: the scheduler's choice)")
	return cmd
}

func resizeCommand() *cobra.Command {
	var cpus int
	var memory, disk string
	cmd := &cobra.Command{
		Use:   "resize <box>",
		Short: "Change one box's CPUs, memory, or disk (a cold boot; agents resume)",
		Long: `Change one box's size. It applies with a cold boot: running processes restart
and agents resume through their adapters. CPU and memory can go up or down; the
disk only grows. If the box no longer fits its worker, it moves to one that fits.`,
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.ResizeParams{ID: args[0], VCPUs: cpus}
			if memory != "" {
				n, err := config.ParseSize(memory)
				if err != nil {
					return fmt.Errorf("--memory: %w", err)
				}
				p.MemMiB = int(n >> 20)
			}
			if disk != "" {
				n, err := config.ParseSize(disk)
				if err != nil {
					return fmt.Errorf("--disk: %w", err)
				}
				p.DiskBytes = n
			}
			if p.VCPUs == 0 && p.MemMiB == 0 && p.DiskBytes == 0 {
				return fmt.Errorf("give --cpu, --memory, or --disk")
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			return rpc.CallOutput(cmd.Context(), sess, api.BoxResize, p, nil, os.Stdout, os.Stderr)
		},
	}
	cmd.Flags().IntVar(&cpus, "cpu", 0, "vCPUs")
	cmd.Flags().StringVar(&memory, "memory", "", "memory, like 16G")
	cmd.Flags().StringVar(&disk, "disk", "", "disk, like 100G (only grows)")
	return cmd
}

// printPlan shows what hutch new would do: where the box goes and why, and what
// it would get.
func printPlan(ctx context.Context, sess *yamux.Session, cfg *config.Config, p api.ForkParams, agent agentFlags) error {
	var plan api.Plan
	if err := rpc.Call(ctx, sess, api.BoxPlan, p, &plan); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	where := plan.Worker
	if where == "" {
		where = "-"
	}
	agentName := agent.agent
	if agentName == "" && agent.cmd == "" {
		agentName = cfg.String("default_agent")
	}
	if agent.cmd != "" {
		agentName = "command: " + agent.cmd
	}
	egress := cfg.String("network.egress")
	if egress == "allowlist" {
		egress += " (" + strings.Join(cfg.List("network.domains"), ", ") + ")"
	}
	allow := strings.Join(p.Allow, ", ")
	if allow == "" {
		allow = "none"
	}
	rows := [][2]string{
		{"Machine", where},
		{"Why", plan.Why},
		{"Start", "warm, from the template snapshot (" + plan.Class + ")"},
		{"Template", plan.Template},
		{"Size", plan.Size},
		{"Agent", agentName},
		{"Autonomy", cfg.String("autonomy")},
		{"Logins", strings.Join(p.Credentials, ", ")},
		{"Egress", egress},
		{"Private targets", allow},
	}
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1])
	}
	tw.Flush()
	for _, w := range plan.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	fmt.Println("Nothing was started (--dry-run).")
	return nil
}
