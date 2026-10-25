package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/server"
	"github.com/mouizahmed/hutch/internal/version"
)

const usage = `usage: hutch <command>

commands:
  server run [--data DIR] [--listen ADDR] [--name NAME]...   run the control plane
  server token [--data DIR]                                   create a worker join token
  api [--data DIR]                                            stdio bridge used by ssh <server> hutch api
  version
`

type names []string

func (n *names) String() string     { return strings.Join(*n, ",") }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

func main() {
	flag.Usage = func() { fmt.Fprint(flag.CommandLine.Output(), usage) }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch {
	case args[0] == "version" || args[0] == "--version":
		fmt.Println("hutch", version.String())
	case args[0] == "api":
		fs := flag.NewFlagSet("api", flag.ExitOnError)
		data := fs.String("data", client.DefaultDataDir(), "server data directory")
		fs.Parse(args[1:])
		err = client.Bridge(server.SocketPath(*data))
	case len(args) > 1 && args[0] == "server" && args[1] == "run":
		fs := flag.NewFlagSet("server run", flag.ExitOnError)
		data := fs.String("data", client.DefaultDataDir(), "data directory")
		listen := fs.String("listen", ":7443", "address workers connect to")
		var ns names
		fs.Var(&ns, "name", "hostname or IP workers use to reach this server (repeatable)")
		fs.Parse(args[2:])
		if len(ns) == 0 {
			host, _ := os.Hostname()
			ns = names{host, "localhost", "127.0.0.1"}
		}
		var s *server.Server
		if s, err = server.New(server.Config{Dir: *data, Listen: *listen, Names: ns}); err == nil {
			log.Printf("hutch server %s, data in %s", version.String(), *data)
			err = s.Run(ctx)
		}
	case len(args) > 1 && args[0] == "server" && args[1] == "token":
		fs := flag.NewFlagSet("server token", flag.ExitOnError)
		data := fs.String("data", client.DefaultDataDir(), "server data directory")
		fs.Parse(args[2:])
		err = createToken(ctx, *data)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hutch:", err)
		os.Exit(1)
	}
}

func createToken(ctx context.Context, dataDir string) error {
	sess, err := client.Dial("", server.SocketPath(dataDir))
	if err != nil {
		return fmt.Errorf("is the server running? %w", err)
	}
	defer sess.Close()
	var t api.Token
	if err := rpc.Call(ctx, sess, api.TokenCreate, struct{}{}, &t); err != nil {
		return err
	}
	fmt.Println(t.Token)
	return nil
}
