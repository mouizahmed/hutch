package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

func doctorCommand() *cobra.Command {
	return doctorCommandFor("")
}

func doctorCommandFor(target string) *cobra.Command {
	return &cobra.Command{
		Use:     "doctor",
		Short:   "Check this computer, the machine, its bucket, boxes, and config, with a fix for each problem",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var checks []api.Check
			local := func(name string, err error, detail, fix string) {
				c := api.Check{Area: "this computer", Name: name, Status: "ok", Detail: detail}
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
			if target == "" {
				target = machineTarget()
			}
			sess, err := connectTarget(target)
			local("machine", err, target, "check that ssh to it works and hutchd runs there (machine in your user config, or $HUTCH_MACHINE)")
			var r api.DoctorResult
			if err == nil {
				defer sess.Close()
				if err := rpc.Call(cmd.Context(), sess, api.Doctor, struct{}{}, &r); err != nil {
					return err
				}
				if r.Version != version.String() {
					checks = append(checks, api.Check{Area: "this computer", Name: "version", Status: "warn",
						Detail: "hutch " + version.String() + ", hutchd " + r.Version, Fix: "use the same Hutch version on both"})
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
				return exitError(1)
			}
			return nil
		},
	}
}
