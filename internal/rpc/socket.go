//go:build !windows

package rpc

import (
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hashicorp/yamux"
)

// ServeSocket serves calls on a Unix socket open to its owner and its
// directory's group, a yamux session per connection, until ctx ends.
func ServeSocket(ctx context.Context, path string, mux Mux, streams Streams) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Dir(path)); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(path, -1, int(st.Gid))
		}
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			cfg := yamux.DefaultConfig()
			cfg.LogOutput = nil
			cfg.Logger = log.New(log.Writer(), "yamux: ", log.LstdFlags)
			sess, err := yamux.Server(conn, cfg)
			if err != nil {
				conn.Close()
				return
			}
			defer sess.Close()
			Serve(ctx, sess, mux, streams)
		}()
	}
}
