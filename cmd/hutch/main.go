package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
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
		Args:          cobra.NoArgs,
		RunE:          runTUI,
		// The TUI opens a project's tab in its checkout, so remember where it is.
		PersistentPreRun: func(*cobra.Command, []string) {
			if repo, _ := config.FindRepo("."); repo != nil && !strings.HasPrefix(repo.ID, "local:") {
				rememberProject(repo.ID, repo.Root)
			}
		},
	}

	root.AddGroup(&cobra.Group{ID: "boxes", Title: "Boxes:"}, &cobra.Group{ID: "projects", Title: "Projects and templates:"},
		&cobra.Group{ID: "setup", Title: "Setup:"})
	root.AddCommand(boxCommands()...)
	root.AddCommand(sessionCommands()...)
	root.AddCommand(threadCommands()...)
	root.AddCommand(gitCommands()...)
	root.AddCommand(handoffCommands()...)
	root.AddCommand(forwardCommands()...)
	root.AddCommand(statusCommand())
	root.AddCommand(setupCommands()...)
	root.AddCommand(moveCommand(), resizeCommand(), doctorCommand())
	root.AddCommand(configCommands(), initCommand(), templateCommands(), projectCommands(), sizesCommand(), userCommands(), loginCommand())

	setHelp(root)

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

// machineTarget is the machine's SSH target: $HUTCH_MACHINE, else hutchd on this
// computer (empty), else the user config's machine key. The user config syncs
// between computers, so on the machine itself its machine key names it from
// elsewhere.
func machineTarget() string {
	if t := os.Getenv("HUTCH_MACHINE"); t != "" {
		return t
	}
	if _, err := os.Stat(client.Socket); err == nil {
		return ""
	}
	d, err := config.ReadFile(config.User, config.UserPath())
	if err != nil {
		return ""
	}
	return config.New(d).String("machine")
}

func dial() (*yamux.Session, error) {
	target := machineTarget()
	sess, err := client.Dial(target, client.Socket)
	if err != nil {
		if target == "" {
			return nil, fmt.Errorf("no machine set (hutch setup <ssh-host>, or machine in %s) and no hutchd on this computer: %w", config.UserPath(), err)
		}
		return nil, fmt.Errorf("connect to %s: %w", target, err)
	}
	return sess, nil
}

// checkVersion refuses a machine whose hutchd is older than this CLI speaks;
// hutchd refuses a CLI that is too old for it.
func checkVersion(ctx context.Context, sess *yamux.Session) error {
	var h api.Handshake
	err := rpc.Call(ctx, sess, api.Hello, api.Handshake{Protocol: api.Protocol, Version: version.String()}, &h)
	switch {
	case err != nil && strings.Contains(err.Error(), "unknown method"):
		return fmt.Errorf("the machine's hutchd is older than hutch %s on this computer: hutch machine upgrade", version.String())
	case err != nil:
		if msg := client.SSHError(); msg != "" && strings.Contains(err.Error(), "EOF") {
			return fmt.Errorf("%s: %s", machineTarget(), msg)
		}
		return err
	case h.Protocol < api.Protocol:
		return fmt.Errorf("the machine's hutchd %s is older than hutch %s on this computer: hutch machine upgrade", h.Version, version.String())
	}
	return nil
}

// connect opens a session to the machine and syncs the user config with it.
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
	if err := checkVersion(ctx, sess); err != nil {
		sess.Close()
		return nil, err
	}
	if err := syncUserConfig(ctx, sess, machineTarget(), ""); err != nil {
		sess.Close()
		if msg := client.SSHError(); msg != "" && strings.Contains(err.Error(), "EOF") {
			return nil, fmt.Errorf("%s: %s", machineTarget(), msg)
		}
		return nil, err
	}
	return sess, nil
}
