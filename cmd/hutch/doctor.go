package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

func doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Short:   "Check the server, bucket, workers, boxes, and config, with a fix for each problem",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var checks []api.Check
			local := func(name string, err error, detail, fix string) {
				c := api.Check{Area: "this machine", Name: name, Status: "ok", Detail: detail}
				if err != nil {
					c.Status, c.Detail, c.Fix = "fail", err.Error(), fix
				}
				checks = append(checks, c)
			}
			_, err := config.ReadFile(config.User, config.UserPath())
			local("user config", err, config.UserPath(), "hutch config edit")
			if repo, err := config.FindRepo("."); err == nil && repo != nil {
				_, err := config.ReadFile(config.Project, repo.ConfigPath())
				local("project config", err, repo.ConfigPath(), "fix "+repo.ConfigPath()+" (hutch config edit --project)")
			}
			sess, err := connect()
			local("server", err, serverTarget(), "check the server is running, and SSH to it works (server in your user config, or $HUTCH_SERVER)")
			var r api.DoctorResult
			if err == nil {
				defer sess.Close()
				if err := rpc.Call(cmd.Context(), sess, api.Doctor, struct{}{}, &r); err != nil {
					return err
				}
				if r.Version != version.String() {
					checks = append(checks, api.Check{Area: "this machine", Name: "version", Status: "warn",
						Detail: "hutch " + version.String() + ", server " + r.Version, Fix: "use the same Hutch version on both"})
				}
				checks = append(checks, r.Checks...)
			}
			failed := false
			area := ""
			for _, c := range checks {
				if c.Area != area {
					area = c.Area
					fmt.Printf("\n%s\n", area)
				}
				mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
				line := "  " + mark + " " + c.Name
				if c.Detail != "" {
					line += ": " + c.Detail
				}
				fmt.Println(line)
				if c.Fix != "" {
					fmt.Println("      fix: " + c.Fix)
				}
				failed = failed || c.Status == "fail"
			}
			if failed {
				os.Exit(1)
			}
			return nil
		},
	}
}
