package main

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

func machineResetCommand() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "reset <ssh-host>",
		Short: "Delete a machine's boxes and setup state, ready for setup again",
		Long: `Delete every local box, snapshot, project, login, setting, and machine
identity on the SSH host. This permanently deletes work kept only there.
Installed runtimes, images, build cache, and the storage mount are kept; the
bucket is untouched. The daemon is stopped and disabled until setup runs again.

Remove a worker with hutch machine rm first. A lead with enrolled workers must
hand over the lead or remove its workers before it can be reset.
Requires --yes; the SSH host is always explicit, even for your configured machine.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("reset permanently deletes all boxes and setup state on %s: repeat with --yes", args[0])
			}
			r := remote{host: args[0]}
			c := r.cmd("sudo hutchd reset --check && sudo systemctl disable --now hutchd && sudo hutchd reset --yes")
			c = exec.CommandContext(cmd.Context(), c.Path, c.Args[1:]...)
			c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			if err := c.Run(); err != nil {
				return fmt.Errorf("reset %s: %w", args[0], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s reset. Start again with hutch setup %s.\n", args[0], args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "permanently delete the machine's boxes and setup state")
	return c
}
