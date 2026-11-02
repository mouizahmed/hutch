package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func machineAddCommand() *cobra.Command {
	var f installFlags
	var name string
	c := &cobra.Command{
		Use:   "add <ssh-host>",
		Short: "Add another machine: it runs boxes as a worker of this one",
		Long: `Add another machine you reach over SSH. It gets hutchd and the machine checks,
then dials this machine (the lead) on its link port, so it needs no open port of
its own. Several machines need a bucket.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			if name == "" {
				out, err := exec.Command("ssh", "-o", "BatchMode=yes", host, "hostname -s").Output()
				if err != nil {
					return fmt.Errorf("ssh %s: %v", host, err)
				}
				name = strings.TrimSpace(string(out))
			}
			var tok api.MachineTokenResult
			if err := rpc.Call(cmd.Context(), sess, api.MachineToken, api.MachineTokenParams{Address: sshHostname(machineTarget()), SSH: host}, &tok); err != nil {
				return err
			}
			r, p, a, st, err := prepare(cmd.Context(), host, f)
			if err != nil {
				return err
			}
			defer a.close()
			if p.get("state") == "yes" {
				return fmt.Errorf("%s has Hutch state of its own in /var/lib/hutch/state: it's a machine of its own, not one to add", host)
			}
			if err := install(r, p, a, st, false); err != nil {
				return err
			}
			fmt.Printf("\nJoining %s at %s\n", name, tok.Lead)
			if err := r.run(fmt.Sprintf("sudo hutchd join --lead %s --token %s --name %s && sudo systemctl restart hutchd",
				shellQuote(tok.Lead), shellQuote(tok.Token), shellQuote(name))); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
			defer cancel()
			for {
				var ws []api.Worker
				if err := rpc.Call(ctx, sess, api.WorkerList, struct{}{}, &ws); err != nil {
					return err
				}
				for _, w := range ws {
					if w.Name == name && !w.Gone && w.Online {
						fmt.Printf("  ✓ %s is connected (%s)\n", name, w.Class)
						return nil
					}
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("%s joined but hasn't connected to %s: the lead's firewall or security group must allow it; see journalctl -u hutchd on %s", name, tok.Lead, host)
				case <-time.After(time.Second):
				}
			}
		},
	}
	f.register(c, false)
	c.Flags().StringVar(&name, "name", "", "the machine's name (default: its hostname)")
	return c
}

func machineRmCommand() *cobra.Command {
	var lost, down bool
	c := &cobra.Command{
		Use:   "rm <name>",
		Short: "Move a machine's boxes to the others, then remove it; it's refused from then on",
		Long: `Move a machine's boxes to the others, then remove it; it's refused from then on.

With --lost, for a machine that's gone: its boxes come back elsewhere from their
newest snapshots in the bucket (work since then is lost). First the machine is
fenced over SSH, so it can't keep running its copies: its hutchd restarts with
every box asleep. If it can't be reached, --down confirms it's off or cut off
from the bucket and the other machines (stopped, unplugged, firewalled); Hutch
can't check that for you, and a machine that's still running its boxes would
keep two copies of each working.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var ws []api.Worker
			if err := rpc.Call(cmd.Context(), sess, api.WorkerList, struct{}{}, &ws); err != nil {
				return err
			}
			i := slices.IndexFunc(ws, func(w api.Worker) bool { return w.Name == args[0] && !w.Gone })
			if i < 0 {
				return fmt.Errorf("no machine named %s", args[0])
			}
			w := ws[i]
			if lost && !w.Online {
				host := cmp.Or(w.SSH, w.Name)
				fmt.Printf("Fencing %s over SSH (%s)...\n", w.Name, host)
				out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host,
					"sudo hutchd fence "+shellQuote("hutch machine rm --lost took its boxes")).CombinedOutput()
				switch {
				case err == nil:
					fmt.Printf("  ✓ fenced: its hutchd restarted with every box asleep\n")
				case !down:
					return fmt.Errorf("can't fence %s over SSH (%v: %s).\nIf it's off or cut off from the bucket and the other machines, run this again with --down; if it's still running its boxes, they'd have two working copies", w.Name, err, strings.TrimSpace(string(out)))
				default:
					fmt.Printf("  ! not fenced (%v); going on, as you said it's down\n", err)
				}
			}
			if err := rpc.CallOutput(cmd.Context(), sess, api.WorkerRm, api.MachineRmParams{ID: w.ID, Lost: lost}, nil, os.Stdout, os.Stderr); err != nil {
				return err
			}
			fmt.Printf("Removed %s; its hutchd can't connect again.\n", w.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&lost, "lost", false, "the machine is gone: bring its boxes back elsewhere from the bucket")
	c.Flags().BoolVar(&down, "down", false, "with --lost: go on when the machine can't be fenced over SSH, as it's off")
	return c
}

// sshHostname is the host an SSH target connects to, as another machine dials it.
func sshHostname(target string) string {
	if target == "" {
		return ""
	}
	out, err := exec.Command("ssh", "-G", target).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			return v
		}
	}
	return ""
}
