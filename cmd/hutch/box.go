package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
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
	newCmd := &cobra.Command{
		Use:     "new",
		Short:   "Create a box from the base snapshot",
		GroupID: "boxes",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
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
			fmt.Printf("%s %s in %s\n", b.ID, b.State, time.Since(start).Round(time.Millisecond))
			return nil
		}),
	}
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
			fmt.Fprintln(tw, "BOX\tSTATE\tSIZE\tWORKER\tCREATED")
			for _, b := range boxes {
				fmt.Fprintf(tw, "%s\t%s\t%d vCPU, %s\t%s\t%s\n", b.ID, b.State, b.VCPUs, mib(b.MemMiB), b.Worker, ago(b.Created))
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
