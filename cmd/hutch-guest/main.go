//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/guest"
	"github.com/mouizahmed/hutch/internal/mcp"
	"github.com/mouizahmed/hutch/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: hutch-guest [--version] init|serve|fingerprint|mcp-check")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("hutch-guest", version.String())
		return
	}

	// The kernel starts it as init with no arguments in Firecracker boxes.
	if os.Getpid() == 1 && flag.NArg() == 0 {
		if err := guest.Init("", "firecracker"); err != nil {
			log.Fatal(err)
		}
		return
	}
	switch flag.Arg(0) {
	case "fingerprint":
		fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
		timeout := fs.Duration("timeout", 0, "how long the whole fingerprint may take")
		fs.Parse(flag.Args()[1:])
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		result, err := guest.Fingerprint(ctx, ".")
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatal(err)
		}
	case "mcp-check":
		fs := flag.NewFlagSet("mcp-check", flag.ExitOnError)
		file := fs.String("file", "", "adapter-declared MCP config file")
		key := fs.String("key", "", "top-level MCP server map")
		var fields agents.MCPFields
		fs.StringVar(&fields.Headers, "headers", "", "the adapter's name for a server's HTTP headers")
		fs.StringVar(&fields.BearerEnv, "bearer-env", "", "its name for the variable holding a bearer token")
		fs.StringVar(&fields.Enabled, "enabled", "", "its name for whether a server is enabled")
		fs.StringVar(&fields.Type, "type", "", "its name for a server's transport type")
		timeout := fs.Duration("timeout", 0, "how long checking every server may take")
		serverTimeout := fs.Duration("server-timeout", 0, "how long one server may take to start and answer")
		fs.Parse(flag.Args()[1:])
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		results, err := mcp.CheckFile(ctx, *file, *key, fields, *serverTimeout)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
			log.Fatal(err)
		}
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		listen := fs.String("listen", "", "tcp:ADDR instead of vsock")
		backend := fs.String("backend", "firecracker", "firecracker or gvisor")
		fs.Parse(flag.Args()[1:])
		if err := guest.Init(*listen, *backend); err != nil {
			log.Fatal(err)
		}
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		listen := fs.String("listen", "", "tcp:ADDR (with $HUTCH_GUEST_TOKEN) or unix:PATH (tests) instead of vsock")
		fs.Parse(flag.Args()[1:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := guest.ListenAndServe(ctx, *listen); err != nil {
			log.Fatal(err)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}
