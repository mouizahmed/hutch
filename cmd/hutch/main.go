package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/version"
)

var flagServerData string

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
	root.PersistentFlags().StringVar(&flagServerData, "server-data", client.DefaultDataDir(), "data directory of a server on this machine")
	root.PersistentFlags().MarkHidden("server-data")

	root.AddGroup(&cobra.Group{ID: "boxes", Title: "Boxes:"}, &cobra.Group{ID: "setup", Title: "Setup:"})
	root.AddCommand(boxCommands()...)
	root.AddCommand(sessionCommands()...)
	root.AddCommand(threadCommands()...)
	root.AddCommand(setupCommands()...)
	root.AddCommand(configCommands())

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

// serverTarget is the server's SSH target: $HUTCH_SERVER, else the user config's
// server key; empty means a server on this machine.
func serverTarget() string {
	if t := os.Getenv("HUTCH_SERVER"); t != "" {
		return t
	}
	d, err := config.ReadFile(config.User, config.UserPath())
	if err != nil {
		return ""
	}
	return config.New(d).String("server")
}

func dial() (*yamux.Session, error) {
	target := serverTarget()
	sess, err := client.Dial(target, server.SocketPath(flagServerData))
	if err != nil {
		if target == "" {
			return nil, fmt.Errorf("no server configured in %s and none running here: %w", config.UserPath(), err)
		}
		return nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	return sess, nil
}

// connect opens a session to the server and syncs the user config with it.
func connect() (*yamux.Session, error) {
	if _, err := config.ReadFile(config.User, config.UserPath()); err != nil {
		return nil, fmt.Errorf("%w\n  fix it with: hutch config edit", err)
	}
	sess, err := dial()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := syncUserConfig(ctx, sess, serverTarget(), ""); err != nil {
		sess.Close()
		return nil, err
	}
	return sess, nil
}
