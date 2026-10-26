package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/sigv4"
	"github.com/mouizahmed/hutch/internal/version"
)

func setupCommands() []*cobra.Command {
	serverCmd := &cobra.Command{Use: "server", Short: "Run and manage the control plane", GroupID: "setup"}

	var listen string
	var names []string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the control plane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(names) == 0 {
				host, _ := os.Hostname()
				names = []string{host, "localhost", "127.0.0.1"}
			}
			s, err := server.New(server.Config{Dir: flagServerData, Listen: listen, Names: names})
			if err != nil {
				return err
			}
			log.Printf("hutch server %s, data in %s", version.String(), flagServerData)
			return s.Run(cmd.Context())
		},
	}
	run.Flags().StringVar(&listen, "listen", ":7443", "address workers connect to")
	run.Flags().StringSliceVar(&names, "name", nil, "hostname or IP workers use to reach this server (repeatable)")

	token := &cobra.Command{
		Use:   "token",
		Short: "Create a single-use worker join token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var t api.Token
			if err := rpc.Call(cmd.Context(), sess, api.TokenCreate, struct{}{}, &t); err != nil {
				return err
			}
			fmt.Println(t.Token)
			return nil
		},
	}
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Connect the server to its bucket and set the recovery passphrase",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, pass, err := bucketForm(true)
			if err != nil {
				return err
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			fmt.Println("Checking the bucket and writing the first backup...")
			if err := rpc.Call(cmd.Context(), sess, api.ServerInit, api.ServerInitParams{Bucket: cfg, Passphrase: pass}, nil); err != nil {
				return err
			}
			fmt.Printf("Done. Everything durable now goes to %s, encrypted.\n", cfg.Name)
			return nil
		},
	}

	restore := &cobra.Command{
		Use:   "restore",
		Short: "Recover a lost server here from its bucket",
		Long: `Recover a lost server on this machine from its bucket: the master key (with your
recovery passphrase), then the newest backup. Then start it with hutch server run;
workers reconnect once the server's address points here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, pass, err := bucketForm(false)
			if err != nil {
				return err
			}
			at, err := server.Restore(cmd.Context(), flagServerData, cfg, pass)
			if err != nil {
				return err
			}
			fmt.Printf("Restored the backup from %s (%s) into %s.\nStart the server: hutch server run\n", at.Local().Format(time.DateTime), ago(at), flagServerData)
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
	serverCmd.AddCommand(run, token, initCmd, restore, gc)

	apiCmd := &cobra.Command{
		Use:    "api",
		Short:  "Bridge stdio to the server (run by ssh <server> hutch api)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE:   func(*cobra.Command, []string) error { return client.Bridge(server.SocketPath(flagServerData)) },
	}

	workers := &cobra.Command{
		Use:     "workers",
		Short:   "List workers",
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
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "WORKER\tNAME\tSTATE\tCLASS\tMEMORY IN USE\tDISK FREE\tCOST\tLAST SEEN")
			for _, w := range ws {
				if w.Gone {
					continue
				}
				state, disk := "offline", "-"
				if !w.Stopped.IsZero() && !w.Online {
					state = "stopped"
				}
				if w.Online {
					state = "online"
					if w.Disk.Total > 0 {
						disk = fmt.Sprintf("%d%% of %d GB", 100*w.Disk.Free/w.Disk.Total, w.Disk.Total>>30)
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s of %s\t%s\t%s\t%s\n", w.ID, w.Name, state, w.Class, mib(w.ReservedMiB), mib(w.MemMiB), disk, perHour(w.PerHour), ago(w.LastSeen))
			}
			return tw.Flush()
		},
	}
	providerCmd := &cobra.Command{Use: "provider", Short: "Let the server start elastic workers when yours are full", GroupID: "setup"}
	var profile string
	addAWS := &cobra.Command{
		Use:   "add aws",
		Short: "Connect the server to AWS (the server must run on EC2)",
		Long: `Connect the server to AWS. New workers go in the server's subnet and security
group, from the newest worker image in the account, and dial the server's private
address. The credentials need EC2 access; they stay on the server.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "aws" {
				return fmt.Errorf("unknown provider %q (known: aws)", args[0])
			}
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return fmt.Errorf("this needs a terminal: credentials are typed, never passed as arguments")
			}
			var c sigv4.Credentials
			err := huh.NewForm(huh.NewGroup(
				huh.NewInput().Title("AWS access key").Value(&c.AccessKey),
				huh.NewInput().Title("AWS secret key").EchoMode(huh.EchoModePassword).Value(&c.SecretKey),
			)).Run()
			if err != nil {
				return err
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			var info api.ProviderInfo
			if err := rpc.Call(cmd.Context(), sess, api.ProviderAdd, api.ProviderParams{Credentials: sigv4.Credentials{
				AccessKey: strings.TrimSpace(c.AccessKey), SecretKey: c.SecretKey}, InstanceProfile: profile}, &info); err != nil {
				return err
			}
			fmt.Printf("AWS connected: region %s, worker image %s; workers dial %s.\n", info.Region, info.Image, info.Address)
			fmt.Println("When your workers are full, the server starts one (providers.aws.worker_type, limits.cloud_workers).")
			return nil
		},
	}
	addAWS.Flags().StringVar(&profile, "instance-profile", "", "IAM instance profile for new workers")
	providerCmd.AddCommand(addAWS)
	var undo bool
	drain := &cobra.Command{
		Use:   "drain <worker>",
		Short: "Stop placing boxes on a worker and move its awake boxes off it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			return rpc.CallOutput(cmd.Context(), sess, api.WorkerDrain, api.DrainParams{Worker: args[0], Undo: undo}, nil, os.Stdout, os.Stderr)
		},
	}
	drain.Flags().BoolVar(&undo, "undo", false, "let the worker take new boxes again")
	workers.AddCommand(drain)
	return []*cobra.Command{serverCmd, apiCmd, workers, providerCmd}
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
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
	passDesc := "Unlocks the bucket on a new server if this one is lost."
	if init {
		passDesc = fmt.Sprintf("At least %d characters. Unlocks the bucket on a new server if this one is lost. Hutch never stores it.", server.MinPassphrase)
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
				if init && len(s) < server.MinPassphrase {
					return fmt.Errorf("at least %d characters", server.MinPassphrase)
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
				Description("Without it, losing this server means the bucket can never be read again.").
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
