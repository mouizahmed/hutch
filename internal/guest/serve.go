//go:build linux

package guest

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/mdlayher/vsock"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/wire"
)

func ListenAndServe(ctx context.Context) error {
	ln, err := vsock.Listen(guestproto.Port, nil)
	if err != nil {
		return fmt.Errorf("vsock listen on port %d: %w", guestproto.Port, err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(ctx, conn)
	}
}

// responder serializes frames written by concurrent output streams.
type responder struct {
	mu   sync.Mutex
	conn net.Conn
}

func (r *responder) send(resp guestproto.Response) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return wire.Write(r.conn, resp)
}

func (r *responder) fail(err error) {
	r.send(guestproto.Response{Type: guestproto.RespError, Error: err.Error()})
}

func handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req guestproto.Request
	if err := wire.Read(conn, &req); err != nil {
		log.Printf("read request: %v", err)
		return
	}

	// The host sends nothing after the request, so a read returning means it hung up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		var b [1]byte
		conn.Read(b[:])
		cancel()
	}()

	r := &responder{conn: conn}
	switch req.Type {
	case guestproto.TypeHello:
		r.send(guestproto.Response{Type: guestproto.RespHello, Version: guestproto.Version, Guest: version.String()})
	case guestproto.TypeRestore:
		if req.Restore == nil {
			r.fail(fmt.Errorf("restore request without body"))
			return
		}
		if err := restore(*req.Restore); err != nil {
			r.fail(err)
			return
		}
		r.send(guestproto.Response{Type: guestproto.RespOK})
	case guestproto.TypeExec:
		if req.Exec == nil || len(req.Exec.Argv) == 0 {
			r.fail(fmt.Errorf("exec request without a command"))
			return
		}
		code, err := execute(ctx, *req.Exec, r)
		if err != nil {
			r.fail(err)
			return
		}
		r.send(guestproto.Response{Type: guestproto.RespExit, ExitCode: code})
	default:
		r.fail(fmt.Errorf("unsupported request type %q", req.Type))
	}
}
