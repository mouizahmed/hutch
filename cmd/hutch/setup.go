package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func setupCommands() []*cobra.Command {
	machineCmd := &cobra.Command{
		Use:     "machine",
		Short:   "Your machine: what it runs and how full it is",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var ws []api.Worker
			if err := rpc.Call(cmd.Context(), sess, api.WorkerList, struct{}{}, &ws); err != nil {
				return err
			}
			boxes, err := listBoxes(cmd.Context(), sess)
			if err != nil {
				return err
			}
			for _, w := range ws {
				if w.Gone {
					continue
				}
				awake, total := 0, 0
				for _, b := range boxes {
					if b.Worker == w.ID && b.Kind != api.BoxVersion {
						total++
						if b.State == "running" {
							awake++
						}
					}
				}
				state, disk := "offline since "+w.LastSeen.Local().Format(time.DateTime), "-"
				if w.Online {
					state = "online"
					if w.Disk.Total > 0 {
						disk = fmt.Sprintf("%d%% free of %d GB", 100*w.Disk.Free/w.Disk.Total, w.Disk.Total>>30)
					}
				}
				target := machineTarget()
				if target == "" {
					target = "this computer"
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				for _, r := range [][2]string{
					{"Machine", w.Name + " (" + target + ")"},
					{"State", state},
					{"hutchd", w.Version},
					{"Class", w.Class},
					{"CPUs", fmt.Sprint(w.CPUs)},
					{"Memory", fmt.Sprintf("%.1f of %.1f GB reserved by awake boxes", float64(w.ReservedMiB)/1024, float64(w.MemMiB)/1024)},
					{"Disk", disk},
					{"Boxes", fmt.Sprintf("%d, %d awake", total, awake)},
				} {
					fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1])
				}
				tw.Flush()
			}
			return nil
		},
	}

	bucketCmd := &cobra.Command{
		Use:   "bucket",
		Short: "Back the machine up to a bucket, and set the recovery passphrase",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var info api.BucketInfo
			if err := rpc.Call(cmd.Context(), sess, api.BucketGet, struct{}{}, &info); err != nil {
				return err
			}
			if info.Name != "" {
				fmt.Printf("The machine already backs up to %s. Enter its details to save them on this computer, for restoring onto a new machine.\n", info.Name)
				return saveExistingBucket(cmd, info.Name)
			}
			cfg, pass, err := bucketForm(true)
			if err != nil {
				return err
			}
			fmt.Println("Checking the bucket and writing the first backup...")
			if err := rpc.Call(cmd.Context(), sess, api.ServerInit, api.ServerInitParams{Bucket: cfg, Passphrase: pass}, nil); err != nil {
				return err
			}
			fmt.Printf("Done. Everything durable now goes to %s, encrypted.\n", cfg.Name)
			if err := saveRecovery(cfg, pass); err != nil {
				return err
			}
			fmt.Printf("Its details are in %s, sealed with your passphrase: restoring onto a new machine asks only for the passphrase.\n", recoveryPath())
			return nil
		},
	}

	gc := &cobra.Command{
		Use:   "gc",
		Short: "Delete old box snapshots and bucket chunks nothing uses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var r api.GCResult
			if err := rpc.Call(cmd.Context(), sess, api.ServerGC, struct{}{}, &r); err != nil {
				return err
			}
			fmt.Printf("%d manifests (%d old snapshots pruned); deleted %d of %d chunks (%d MB)\n", r.Manifests, r.Pruned, r.Deleted, r.Chunks, r.Bytes>>20)
			return nil
		},
	}
	identity := &cobra.Command{
		Use: "identity", Short: "Print the machine's issuer and public keys for verifying box identities", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var public keys.PublicIdentity
			if err := rpc.Call(cmd.Context(), sess, api.KeysIdentity, struct{}{}, &public); err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(public)
		},
	}
	machineCmd.AddCommand(bucketCmd, gc, upgradeCommand(), restoreCommand(), addComputerCommand(), joinCommand(), identity)
	return []*cobra.Command{setupCommand(), machineCmd}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return ""
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// bucketForm asks for the bucket and the recovery passphrase; at init the passphrase
// is new, so it is asked twice and its safekeeping confirmed.
func bucketForm(init bool) (bucket.Config, string, error) {
	cfg := bucket.Config{Region: "us-east-1"}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return cfg, "", fmt.Errorf("this needs a terminal: the recovery passphrase is typed, never passed as an argument")
	}
	var pass, again string
	stored := false
	notEmpty := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("required")
		}
		return nil
	}
	passDesc := "Unlocks the bucket on a new machine if this one is lost."
	if init {
		passDesc = fmt.Sprintf("At least %d characters. Unlocks the bucket on a new machine if this one is lost. Hutch never stores it.", api.MinPassphrase)
	}
	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Bucket").Description("An S3-compatible bucket with conditional writes, empty at init.").Value(&cfg.Name).Validate(notEmpty),
			huh.NewInput().Title("Endpoint").Description("Blank for AWS S3; otherwise https://host[:port] (R2, MinIO, Garage).").Value(&cfg.Endpoint),
			huh.NewInput().Title("Region").Value(&cfg.Region).Validate(notEmpty),
			huh.NewInput().Title("Access key").Value(&cfg.AccessKey).Validate(notEmpty),
			huh.NewInput().Title("Secret key").EchoMode(huh.EchoModePassword).Value(&cfg.SecretKey).Validate(notEmpty),
		),
		huh.NewGroup(huh.NewInput().Title("Recovery passphrase").Description(passDesc).EchoMode(huh.EchoModePassword).Value(&pass).
			Validate(func(s string) error {
				if init && len(s) < api.MinPassphrase {
					return fmt.Errorf("at least %d characters", api.MinPassphrase)
				}
				return notEmpty(s)
			})),
	}
	if init {
		groups = append(groups,
			huh.NewGroup(huh.NewInput().Title("Passphrase again").EchoMode(huh.EchoModePassword).Value(&again).
				Validate(func(s string) error {
					if s != pass {
						return fmt.Errorf("doesn't match")
					}
					return nil
				})),
			huh.NewGroup(huh.NewConfirm().Title("Have you stored the passphrase somewhere safe?").
				Description("Without it, losing this machine means the bucket can never be read again.").
				Affirmative("Yes, it's stored").Negative("Cancel").Value(&stored)))
	}
	if err := huh.NewForm(groups...).Run(); err != nil {
		return cfg, "", err
	}
	if init && !stored {
		return cfg, "", fmt.Errorf("cancelled")
	}
	cfg.Name, cfg.Endpoint, cfg.Region, cfg.AccessKey = strings.TrimSpace(cfg.Name), strings.TrimSpace(cfg.Endpoint), strings.TrimSpace(cfg.Region), strings.TrimSpace(cfg.AccessKey)
	return cfg, pass, nil
}
