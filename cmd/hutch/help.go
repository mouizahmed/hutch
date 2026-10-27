package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// coreCommands are all hutch --help shows; changing them needs a decisions.md entry.
var coreCommands = []string{"new", "cloud", "local", "ls", "attach", "pr", "rm"}

// setHelp makes hutch --help show the core commands, and hutch help more the rest.
func setHelp(root *cobra.Command) {
	full := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if cmd != root {
			full(cmd, args)
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n\nUsage:\n  hutch                    open the TUI\n  hutch <command>\n\nCommands:\n", root.Short)
		for _, name := range coreCommands {
			if c, _, err := root.Find([]string{name}); err == nil && c != root {
				fmt.Fprintf(&b, "  %-10s %s\n", c.Name(), c.Short)
			}
		}
		b.WriteString("\nEvery other command: hutch help more\nAbout one: hutch <command> --help\n")
		fmt.Fprint(cmd.OutOrStdout(), b.String())
	})
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command | more]",
		Short: "Help about a command; more lists every command",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] == "more" {
				full(root, nil)
				return nil
			}
			c, _, err := root.Find(args)
			if err != nil {
				return err
			}
			return c.Help()
		},
	})
}
