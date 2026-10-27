package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

func forwardCommands() []*cobra.Command {
	forward := &cobra.Command{
		Use:     "forward <box> [<port>[:<local port>]...]",
		Short:   "Reach a box's ports on localhost; with no ports, each port it listens on",
		GroupID: "boxes",
		Args:    cobra.MinimumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			box := args[0]
			if err := ensureAwake(ctx, sess, box); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			add := func(remote, local int) {
				ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", local))
				if err != nil {
					fmt.Fprintf(os.Stderr, "can't forward %s:%d: %v\n", box, remote, err)
					return
				}
				go func() { <-ctx.Done(); ln.Close() }()
				fmt.Printf("localhost:%d → %s:%d\n", local, box, remote)
				go serveForward(ctx, sess, box, remote, ln)
			}
			if len(args) > 1 {
				for _, a := range args[1:] {
					r, l, ok := strings.Cut(a, ":")
					if !ok {
						l = r
					}
					remote, err1 := strconv.Atoi(r)
					local, err2 := strconv.Atoi(l)
					if err1 != nil || err2 != nil {
						return fmt.Errorf("invalid port %q: want <port> or <port>:<local port>", a)
					}
					add(remote, local)
				}
				fmt.Println("Ctrl-C to stop.")
				<-ctx.Done()
				return nil
			}
			fmt.Println("Forwarding each port the box listens on, as it starts. Ctrl-C to stop.")
			done := map[int]bool{22: true}
			for ctx.Err() == nil {
				var st guestproto.Status
				if err := rpc.Call(ctx, sess, api.BoxStatus, api.IDParams{ID: box}, &st); err == nil {
					var ports []int
					for _, p := range st.Ports {
						if !done[p.Port] {
							ports = append(ports, p.Port)
						}
					}
					sort.Ints(ports)
					for _, p := range ports {
						done[p] = true
						add(p, p)
					}
				}
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
				}
			}
			return nil
		}),
	}

	var printOnly bool
	sshConfig := &cobra.Command{
		Use:     "ssh-config",
		Short:   "Add <box>.hutch hosts to ~/.ssh/config, for ssh, VS Code, and JetBrains",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := selfPath()
			if err != nil {
				return err
			}
			if _, err := sshKey(); err != nil {
				return err
			}
			block := sshBlock(exe, client.SSHKeyPath())
			if printOnly {
				fmt.Print(block)
				return nil
			}
			home, _ := os.UserHomeDir()
			path := filepath.Join(home, ".ssh", "config")
			cur, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			next := sshManaged.ReplaceAllLiteralString(string(cur), "")
			next = strings.TrimRight(next, "\n")
			if next != "" {
				next += "\n\n"
			}
			next += block
			if next == string(cur) {
				fmt.Printf("%s is up to date.\n", path)
				return nil
			}
			fmt.Printf("Adding to %s:\n\n%s\n", path, block)
			if !term.IsTerminal(int(os.Stdin.Fd())) || !confirm("Write it?") {
				return errors.New("not written (--print shows the entry)")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
				return err
			}
			fmt.Println("Written. ssh <box>.hutch, or connect to <box>.hutch in VS Code or JetBrains.")
			return nil
		},
	}
	sshConfig.Flags().BoolVar(&printOnly, "print", false, "print the entry instead of writing it")

	proxy := &cobra.Command{
		Use:    "ssh-proxy <box>.hutch",
		Short:  "Connect stdin and stdout to a box's SSH server (ssh's ProxyCommand)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			box := strings.TrimSuffix(args[0], ".hutch")
			pub, err := sshKey()
			if err != nil {
				return err
			}
			// Authorizing the key on each connection also wakes the box.
			script := `mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && { grep -qxF "$1" ~/.ssh/authorized_keys || echo "$1" >> ~/.ssh/authorized_keys; }`
			code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Argv: []string{"sh", "-c", script, "sh", strings.TrimSpace(string(pub))}}, io.Discard, os.Stderr)
			if err == nil && code != 0 {
				err = fmt.Errorf("authorize key: exit %d", code)
			}
			if err != nil {
				return err
			}
			conn, err := rpc.OpenStream(ctx, sess, api.BoxDial, api.DialParams{ID: box, Port: 22})
			if err != nil {
				return err
			}
			defer conn.Close()
			var r guestproto.Response
			if err := wire.Read(conn, &r); err != nil {
				return err
			}
			if r.Type != guestproto.RespOK {
				return fmt.Errorf("%s's SSH server: %s", box, r.Error)
			}
			go func() {
				io.Copy(conn, os.Stdin)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
			}()
			_, err = io.Copy(os.Stdout, conn)
			return err
		}),
	}
	return []*cobra.Command{forward, sshConfig, proxy}
}

var sshManaged = regexp.MustCompile(`(?s)\n*# BEGIN hutch.*?# END hutch\n?`)

// sshBlock is the ~/.ssh/config entry for every box. The connection runs through
// Hutch's authenticated relay to the box itself, so boxes' host keys, which change
// with every new box of a name, aren't checked.
func sshBlock(exe, key string) string {
	return fmt.Sprintf(`# BEGIN hutch (written by hutch ssh-config)
Host *.hutch
  User dev
  ProxyCommand "%s" ssh-proxy %%h
  IdentityFile "%s"
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
# END hutch
`, exe, key)
}

// ensureAwake wakes a sleeping box.
func ensureAwake(ctx context.Context, sess *yamux.Session, box string) error {
	b, err := findBox(ctx, sess, box)
	if err != nil {
		return err
	}
	if b.State == "asleep" || b.State == "archived" {
		fmt.Printf("Waking %s...\n", box)
		return rpc.Call(ctx, sess, api.BoxWake, api.IDParams{ID: box}, nil)
	}
	return nil
}
