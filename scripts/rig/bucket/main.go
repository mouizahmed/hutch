// Command bucket gives a test machine a bucket without the interactive form:
// {"bucket": ..., "passphrase": ...} on stdin, the machine's SSH target as its
// argument.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bucket:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: bucket <ssh-target> < params.json")
	}
	var p api.ServerInitParams
	if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
		return err
	}
	sess, err := client.Dial(os.Args[1], client.Socket)
	if err != nil {
		return err
	}
	defer sess.Close()
	if err := rpc.Call(context.Background(), sess, api.ServerInit, p, nil); err != nil {
		return err
	}
	fmt.Printf("%s backs up to %s\n", os.Args[1], p.Bucket.Name)
	return nil
}
