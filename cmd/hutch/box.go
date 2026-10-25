package main

import (
	"context"
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
	"github.com/mouizahmed/hutch/internal/names"
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
	var name, from string
	var agent agentFlags
	newCmd := &cobra.Command{
		Use:     `new ["<prompt>"]`,
		Short:   "Create a box and start an agent in it",
		GroupID: "boxes",
		Args:    cobra.MaximumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			if name == "" {
				boxes, err := listBoxes(ctx, sess)
				if err != nil {
					return err
				}
				taken := map[string]bool{}
				for _, b := range boxes {
					taken[b.ID] = true
				}
				name = names.New(taken)
			}
			start := time.Now()
			var b api.Box
			if err := rpc.Call(ctx, sess, api.BoxFork, api.ForkParams{Source: from, ID: name}, &b); err != nil {
				return err
			}
			t, err := startThread(ctx, sess, b.ID, agent, args)
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
	newCmd.Flags().StringVar(&from, "from", "base", "sleeping box to fork from")

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
			fmt.Fprintln(tw, "BOX / THREAD\tAGENT\tSTATE\tSIZE\tWORKER\tCREATED")
			for _, b := range boxes {
				fmt.Fprintf(tw, "%s\t\t%s\t%d vCPU, %s\t%s\t%s\n", b.ID, b.State, b.VCPUs, mib(b.MemMiB), b.Worker, ago(b.Created))
				for _, t := range b.Threads {
					fmt.Fprintf(tw, "  %d  %s\t%s\t\t\t\t%s\n", t.N, t.Title, t.Agent, ago(t.Created))
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

	var follow bool
	console := &cobra.Command{
		Use:     "console <box>",
		Short:   "Show a box's serial console (works when the guest is broken)",
		GroupID: "boxes",
		Args:    cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			_, err := rpc.Exec(ctx, sess, api.BoxConsole, api.ConsoleParams{ID: args[0], Follow: follow}, os.Stdout, os.Stderr)
			if ctx.Err() != nil {
				return nil
			}
			return err
		}),
	}
	console.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output")

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
			if err := rpc.Call(ctx, sess, api.BoxRm, api.IDParams{ID: args[0]}, nil); err != nil {
				return err
			}
			fmt.Println(args[0], "removed")
			return nil
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
	c.Flags().StringVar(&a.agent, "agent", "", "agent to run: "+strings.Join(agents.Names(), ", ")+" (default: default_agent, else claude)")
	c.Flags().StringVar(&a.cmd, "cmd", "", "run any terminal command instead of an agent")
	c.MarkFlagsMutuallyExclusive("agent", "cmd")
}

func startThread(ctx context.Context, sess *yamux.Session, box string, a agentFlags, args []string) (api.Thread, error) {
	p := api.ThreadParams{Box: box, Agent: a.agent, Cmd: a.cmd}
	if len(args) > 0 {
		p.Prompt = args[0]
	}
	if p.Agent == "" && p.Cmd == "" {
		cfg, err := config.Load()
		if err != nil {
			return api.Thread{}, err
		}
		p.Agent = cfg.DefaultAgent
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
			t, err := startThread(ctx, sess, args[0], agent, args[1:])
			if err != nil {
				return err
			}
			fmt.Printf("%s/%d %s started\n  hutch attach %s/%d\n", t.Box, t.N, t.Agent, t.Box, t.N)
			return nil
		}),
	}
	agent.register(thread)

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
