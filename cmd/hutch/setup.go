package main

import (
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/server"
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
	serverCmd.AddCommand(run, token)

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
			fmt.Fprintln(tw, "WORKER\tNAME\tSTATE\tCLASS\tLAST SEEN")
			for _, w := range ws {
				state := "offline"
				if w.Online {
					state = "online"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", w.ID, w.Name, state, w.Class, ago(w.LastSeen))
			}
			return tw.Flush()
		},
	}
	return []*cobra.Command{serverCmd, apiCmd, workers}
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
