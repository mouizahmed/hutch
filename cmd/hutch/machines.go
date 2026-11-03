package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/link"
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
			if err := leftoverBoxes(p, host, tok.Token); err != nil {
				return err
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
			unfenced := false
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
					unfenced = true
				}
			}
			if err := rpc.CallOutput(cmd.Context(), sess, api.WorkerRm, api.MachineRmParams{ID: w.ID, Lost: lost, Down: unfenced}, nil, os.Stdout, os.Stderr); err != nil {
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

func machineLeadCommand() *cobra.Command {
	var fromBackup, down bool
	c := &cobra.Command{
		Use:   "lead <machine>",
		Short: "Make another machine the lead",
		Long: `Make another machine the lead.

<machine> is a connected worker's name: changes and login refreshes stop for
seconds, the lead hands it the database and master key, and the old lead
becomes its worker. Boxes keep running; nothing is lost.

With --from-backup, for a lead that's dead, <machine> is the SSH host of a
worker to promote: it restores the lead's newest backup from the bucket, with
your recovery passphrase, and leads. Up to backup.interval of changes can be
lost. The old lead is fenced over SSH first, so it can't keep leading; if it
can't be reached, --down confirms it's off or cut off from the bucket and the
other machines. Its boxes come back elsewhere when used, and it rejoins as a
worker with hutch machine add.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var host string
			if fromBackup {
				host = args[0]
				if err := promote(cmd, host, down); err != nil {
					return err
				}
			} else {
				var err error
				if host, err = handOver(cmd, args[0]); err != nil {
					return err
				}
			}
			if err := setUserMachine(host); err != nil {
				return err
			}
			fmt.Printf("machine = %q in %s\n", host, config.UserPath())
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			for {
				if sess, err := dialTarget(host); err == nil {
					var h api.Handshake
					err = rpc.Call(ctx, sess, api.Hello, api.Handshake{Protocol: api.Protocol}, &h)
					sess.Close()
					if err == nil && h.LeadDown == "" {
						fmt.Printf("  ✓ %s leads\n", host)
						return nil
					}
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("%s hasn't started leading yet: see journalctl -u hutchd there", host)
				case <-time.After(2 * time.Second):
				}
			}
		},
	}
	c.Flags().BoolVar(&fromBackup, "from-backup", false, "the lead is dead: promote <machine> (its SSH host) from the bucket's newest backup")
	c.Flags().BoolVar(&down, "down", false, "with --from-backup: go on when the old lead can't be fenced over SSH, as it's off")
	return c
}

// handOver asks the lead to hand over to the named worker, and returns the SSH
// host to reach it at.
func handOver(cmd *cobra.Command, name string) (string, error) {
	sess, err := connect()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var ws []api.Worker
	if err := rpc.Call(cmd.Context(), sess, api.WorkerList, struct{}{}, &ws); err != nil {
		return "", err
	}
	i := slices.IndexFunc(ws, func(w api.Worker) bool { return w.Name == name && !w.Gone })
	if i < 0 {
		return "", fmt.Errorf("no machine named %s", name)
	}
	w := ws[i]
	host := cmp.Or(w.SSH, w.Name)
	addr := sshHostname(host)
	if addr == "" {
		return "", fmt.Errorf("can't tell %s's address from its SSH host %s", w.Name, host)
	}
	fmt.Printf("Handing the lead to %s (%s)...\n", w.Name, addr)
	if err := rpc.CallOutput(cmd.Context(), sess, api.MachineLead, api.MachineLeadParams{ID: w.ID, Address: addr, Self: machineTarget()}, nil, os.Stdout, os.Stderr); err != nil {
		return "", err
	}
	return host, nil
}

// promote makes the worker at host the lead from the bucket's newest backup,
// after fencing the old lead.
func promote(cmd *cobra.Command, host string, down bool) error {
	cfg, pass, err := recoverBucket()
	if err != nil {
		return err
	}
	old := machineTarget()
	if old != "" && old != host {
		fmt.Printf("Fencing the old lead over SSH (%s)...\n", old)
		out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", old,
			"sudo hutchd fence "+shellQuote("hutch machine lead --from-backup made "+host+" the lead")).CombinedOutput()
		switch {
		case err == nil:
			fmt.Println("  ✓ fenced: it restarted with its boxes asleep and its state aside")
		case !down:
			return fmt.Errorf("can't fence the old lead %s over SSH (%v: %s).\nIf it's off or cut off from the bucket and the other machines, run this again with --down; if it still leads, there'd be two leads until it reads the new lead record", old, err, strings.TrimSpace(string(out)))
		default:
			fmt.Printf("  ! not fenced (%v); going on, as you said it's down\n", err)
			b, err := bucket.New(cfg)
			if err != nil {
				return err
			}
			if err := link.StillAlive(cmd.Context(), b, "lead", "the old lead "+old); err != nil {
				return err
			}
		}
	}
	addr := sshHostname(host)
	if addr == "" {
		return fmt.Errorf("can't tell %s's address", host)
	}
	body, _ := json.Marshal(api.ServerInitParams{Bucket: cfg, Passphrase: pass, Address: addr})
	fmt.Printf("Restoring the newest backup onto %s...\n", host)
	c := exec.Command("ssh", "-o", "BatchMode=yes", host, "sudo hutchd restore && sudo systemctl restart hutchd")
	var out bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(body), &out, &out
	if err := c.Run(); err != nil {
		return fmt.Errorf("restore on %s: %v: %s", host, err, strings.TrimSpace(out.String()))
	}
	fmt.Print("  ✓ " + out.String())
	return nil
}

func forgetStaleCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget-stale <ssh-host>",
		Short: "Delete the lead state an old lead kept aside, once you've checked the new lead",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := exec.Command("ssh", "-o", "BatchMode=yes", args[0],
				`sudo sh -c 'ls -d /var/lib/hutch/state/stale-lead-* 2>/dev/null && rm -rf /var/lib/hutch/state/stale-lead-*; true'`).CombinedOutput()
			if err != nil {
				return fmt.Errorf("ssh %s: %v: %s", args[0], err, out)
			}
			if len(bytes.TrimSpace(out)) == 0 {
				fmt.Printf("%s keeps no old lead state.\n", args[0])
				return nil
			}
			fmt.Printf("Deleted on %s:\n%s", args[0], out)
			return nil
		},
	}
}

// leftoverBoxes refuses a machine holding boxes from another Hutch setup: as a
// worker they would be adopted as this lead's. A machine of this setup before
// (a worker trusting this lead's CA, or an old lead keeping its state aside) is
// fine: its copies are reconciled, and stale ones kept asleep as <box>-returned.
func leftoverBoxes(p probe, host, token string) error {
	boxes := strings.Fields(p.get("boxes"))
	if len(boxes) == 0 {
		return nil
	}
	fp, _, _ := strings.Cut(token, ".")
	if slices.Contains(strings.Fields(p.get("link_ca")), fp) {
		return nil
	}
	return fmt.Errorf("%s holds boxes from another Hutch setup (%s); as a worker here they'd be taken for this machine's.\n"+
		"Move their work somewhere you keep it, then remove them on %s with sudo hutchd box rm <box>, and add it again",
		host, strings.Join(boxes, ", "), host)
}
