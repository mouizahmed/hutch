package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/hashicorp/yamux"
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
	var template bool
	c := &cobra.Command{
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
				var p api.DoctorParams
				if cfg, err := loadConfig(cmd.Context(), sess); err == nil {
					p.DefaultAgent = cfg.String("default_agent")
				}
				if err := rpc.Call(cmd.Context(), sess, api.Doctor, p, &r); err != nil {
					return err
				}
				if r.Version != version.String() {
					checks = append(checks, api.Check{Area: "this computer", Name: "version", Status: "warn",
						Detail: "hutch " + version.String() + ", hutchd " + r.Version, Fix: "use the same Hutch version on both"})
				}
				checks = append(checks, r.Checks...)
				checks = linkFromWorkers(cmd.Context(), sess, checks)
				if template {
					var result api.DoctorResult
					checkErr := func() error {
						repo, cfg, err := project(cmd.Context(), sess)
						if err != nil {
							return err
						}
						p, err := templateParams(repo, cfg, true)
						if err != nil {
							return err
						}
						return rpc.CallOutput(cmd.Context(), sess, api.DoctorTemplate, p, &result, os.Stdout, os.Stderr)
					}()
					if checkErr != nil {
						checks = append(checks, api.Check{Area: "template", Name: "check", Status: "fail", Detail: checkErr.Error(), Fix: "commit and push the recipe; run hutch init if this project is not registered; upgrade hutchd if it lacks this check"})
					} else {
						checks = append(checks, result.Checks...)
					}
				}
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
	c.Flags().BoolVar(&template, "template", false, "build the committed project twice in temporary boxes, compare fingerprints, and check declared MCP servers")
	return c
}

// linkFromWorkers tries the lead's link port from each worker that isn't
// connected, over SSH: a firewall or security group between them is the usual
// reason. Its result goes with that machine's checks.
func linkFromWorkers(ctx context.Context, sess *yamux.Session, checks []api.Check) []api.Check {
	var ws []api.Worker
	if rpc.Call(ctx, sess, api.WorkerList, struct{}{}, &ws) != nil {
		return checks
	}
	live := 0
	for _, w := range ws {
		if !w.Gone {
			live++
		}
	}
	for _, w := range ws {
		if w.Gone || w.Online || w.Lead || live < 2 {
			continue
		}
		c := api.Check{Area: "machine " + w.Name, Name: "link port from there", Status: "ok"}
		out, err := remote{cmp.Or(w.SSH, w.Name)}.cmd(linkProbe).CombinedOutput()
		lead := strings.TrimSpace(string(out))
		switch {
		case err == nil:
			c.Detail = "reaches the lead at " + lead
		case strings.HasPrefix(lead, "unreachable "):
			c.Status, c.Detail = "fail", "can't reach the lead at "+strings.TrimPrefix(lead, "unreachable ")
			c.Fix = "open the lead's link port (machine.link_port) to this machine in the lead's firewall or security group"
		default:
			c.Status, c.Detail = "warn", "couldn't test it over SSH ("+cmp.Or(lead, err.Error())+")"
			c.Fix = "make ssh " + cmp.Or(w.SSH, w.Name) + " work, or check the machine is running"
		}
		i := len(checks)
		for j := range checks {
			if checks[j].Area == c.Area {
				i = j + 1
			}
		}
		checks = slices.Insert(checks, i, c)
	}
	return checks
}

// linkProbe prints the lead address a worker dials, and fails, printing
// "unreachable <address>", when it can't open a connection to it.
const linkProbe = `lead=$(sudo -n cat /var/lib/hutch/link/lead)
if timeout 5 bash -c "exec 3<>/dev/tcp/${lead%:*}/${lead##*:}" 2>/dev/null; then echo "$lead"; else echo "unreachable $lead"; exit 1; fi`
