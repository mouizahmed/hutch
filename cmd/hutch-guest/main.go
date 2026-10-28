//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mouizahmed/hutch/internal/guest"
	"github.com/mouizahmed/hutch/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: hutch-guest [--version] init|serve")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("hutch-guest", version.String())
		return
	}

	switch flag.Arg(0) {
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
