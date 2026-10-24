package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mouizahmed/hutch/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: hutch-guest [--version]")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("hutch-guest", version.String())
		return
	}

	flag.Usage()
	os.Exit(2)
}
