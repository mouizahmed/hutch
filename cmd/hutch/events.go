package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

func eventsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "Print box, terminal, and worker changes as JSON lines",
		Long: `Print box, thread, and worker changes as JSON lines: first everything as it is
now, then a {"type":"synced"} line, then each change as it happens. Types:
box, thread, worker (the whole object as it is now), and box_removed,
thread_removed, worker_removed (with id, and n for a thread).`,
		GroupID: "boxes",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			enc := json.NewEncoder(os.Stdout)
			return watchEvents(ctx, sess, func(e api.Event) error { return enc.Encode(e) })
		}),
	}
}

// watchEvents calls f with each event from the machine until the stream ends.
func watchEvents(ctx context.Context, sess *yamux.Session, f func(api.Event) error) error {
	conn, err := rpc.OpenStream(ctx, sess, api.Events, struct{}{})
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	for {
		var e struct {
			api.Event
			Error string `json:"error"`
		}
		if err := wire.Read(conn, &e); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if e.Type == rpc.FrameError {
			return errors.New(e.Error)
		}
		if err := f(e.Event); err != nil {
			return err
		}
	}
}
