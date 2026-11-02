// Command bucket gives a test machine a bucket without the interactive form:
// {"bucket": ..., "passphrase": ...} on stdin, the machine's SSH target as its
// argument. It also saves the CLI's recovery details, sealed with the
// passphrase, as hutch machine bucket does; with --recovery, only those.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/durable"
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
		return fmt.Errorf("usage: bucket <ssh-target> | --recovery < params.json")
	}
	var p api.ServerInitParams
	if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
		return err
	}
	plain, _ := json.Marshal(p.Bucket)
	path := filepath.Join(filepath.Dir(config.UserPath()), "recovery.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, durable.WrapBytes("laptop/bucket", plain, p.Passphrase).Marshal(), 0o600); err != nil {
		return err
	}
	if os.Args[1] == "--recovery" {
		fmt.Printf("saved %s\n", path)
		return nil
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
