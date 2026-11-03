package rpc

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/wire"
)

func TestOpenStreamCancelUnblocksRead(t *testing.T) {
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	client, err := yamux.Client(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := yamux.Server(b, cfg)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := OpenStream(ctx, client, "wait", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := server.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var req Request
	if err := wire.ReadMax(peer, &req, wire.MaxRPCFrame); err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		var frame Frame
		readDone <- wire.ReadMax(conn, &frame, wire.MaxRPCFrame)
	}()
	cancel()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("read succeeded after its context was canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock the stream read")
	}
}
