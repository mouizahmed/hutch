package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/version"
)

var (
	flagServer     string
	flagServerData string
)

// exitError carries a remote command's exit code back to main.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	root := &cobra.Command{
		Use:           "hutch",
		Short:         "Every coding agent gets its own computer",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagServer, "server", "", "SSH target of the server (overrides config)")
	root.PersistentFlags().StringVar(&flagServerData, "server-data", client.DefaultDataDir(), "data directory of a server on this machine")
	root.PersistentFlags().MarkHidden("server-data")

	root.AddGroup(&cobra.Group{ID: "boxes", Title: "Boxes:"}, &cobra.Group{ID: "setup", Title: "Setup:"})
	root.AddCommand(boxCommands()...)
	root.AddCommand(sessionCommands()...)
	root.AddCommand(threadCommands()...)
	root.AddCommand(setupCommands()...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := root.ExecuteContext(ctx)
	stop()
	var code exitError
	switch {
	case errors.As(err, &code):
		os.Exit(int(code))
	case err != nil:
		fmt.Fprintln(os.Stderr, "hutch:", err)
		os.Exit(1)
	}
}

// connect opens a session to the configured server.
func connect() (*yamux.Session, error) {
	target := flagServer
	if target == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		target = cfg.Server
	}
	sess, err := client.Dial(target, server.SocketPath(flagServerData))
	if err != nil {
		if target == "" {
			return nil, fmt.Errorf("no server configured in %s and none running here: %w", config.Path(), err)
		}
		return nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	return sess, nil
}
