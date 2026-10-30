//go:build linux

package guest

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/wire"
)

// ListenAndServe serves on vsock (Firecracker boxes), on TCP when listen is
// "tcp:ADDR" (gVisor boxes, which have no vsock; clients present the token), or on
// a Unix socket when listen is "unix:PATH" (the protocol compatibility tests).
func ListenAndServe(ctx context.Context, listen string) error {
	var ln net.Listener
	var err error
	token := os.Getenv(TokenEnv)
	if path, ok := strings.CutPrefix(listen, "unix:"); ok {
		ln, err = net.Listen("unix", path)
	} else if addr, ok := strings.CutPrefix(listen, "tcp:"); ok {
		if token == "" {
			return fmt.Errorf("listening on TCP needs %s", TokenEnv)
		}
		ln, err = net.Listen("tcp", addr)
	} else {
		ln, err = vsock.Listen(guestproto.Port, nil)
	}
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go restoreThreads()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if _, ok := ln.(*net.TCPListener); ok {
			go func() {
				if checkToken(conn, token) {
					handle(ctx, conn)
				}
			}()
			continue
		}
		go handle(ctx, conn)
	}
}

// checkToken reads the client's "TOKEN <t>" line and answers "OK" when it matches.
func checkToken(conn net.Conn, token string) bool {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := guestproto.ReadLine(conn)
	conn.SetReadDeadline(time.Time{})
	got, ok := strings.CutPrefix(line, "TOKEN ")
	if err != nil || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		conn.Close()
		return false
	}
	if _, err := conn.Write([]byte("OK hutch-guest\n")); err != nil {
		conn.Close()
		return false
	}
	return true
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

var open = struct {
	sync.Mutex
	conns map[net.Conn]bool
}{conns: map[net.Conn]bool{}}

func track(conn net.Conn, on bool) {
	open.Lock()
	defer open.Unlock()
	if on {
		open.conns[conn] = true
	} else {
		delete(open.conns, conn)
	}
}

func closeOthers(keep net.Conn) {
	open.Lock()
	defer open.Unlock()
	for c := range open.conns {
		if c != keep {
			c.Close()
		}
	}
}

func handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req guestproto.Request
	if err := wire.Read(conn, &req); err != nil {
		log.Printf("read request: %v", err)
		return
	}

	if req.Type == guestproto.TypeStatus {
		var p guestproto.StatusParams
		if req.Status != nil {
			p = *req.Status
		}
		st, err := status(p)
		r := &responder{conn: conn}
		if err != nil {
			r.fail(err)
			return
		}
		r.send(guestproto.Response{Type: guestproto.RespStatus, Status: &st})
		return
	}
	sessions.Add(1)
	defer sessions.Add(-1)
	track(conn, true)
	defer track(conn, false)

	if req.Type == guestproto.TypeDial {
		if req.Dial == nil {
			(&responder{conn: conn}).fail(fmt.Errorf("dial request without a port"))
			return
		}
		dial(conn, *req.Dial)
		return
	}

	if req.Type == guestproto.TypePTY {
		if req.PTY == nil || len(req.PTY.Argv) == 0 {
			(&responder{conn: conn}).fail(fmt.Errorf("pty request without a command"))
			return
		}
		if err := runPTY(conn, *req.PTY); err != nil {
			(&responder{conn: conn}).fail(err)
		}
		return
	}

	// The host sends nothing after the request but a command's input, so a read
	// returning (or failing) means it hung up.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stdin io.Reader
	if req.Type == guestproto.TypeExec && req.Exec != nil && req.Exec.Stdin {
		inR, inW := io.Pipe()
		stdin = inR
		go func() {
			for {
				var in guestproto.Input
				if err := wire.Read(conn, &in); err != nil {
					inW.Close()
					cancel()
					return
				}
				switch in.Type {
				case guestproto.InputStdin:
					inW.Write(in.Data)
				case guestproto.InputEOF:
					inW.Close()
				}
			}
		}()
	} else {
		go func() {
			var b [1]byte
			conn.Read(b[:])
			cancel()
		}()
	}

	r := &responder{conn: conn}
	switch req.Type {
	case guestproto.TypeHello:
		r.send(guestproto.Response{Type: guestproto.RespHello, Version: guestproto.Version, Guest: version.String()})
	case guestproto.TypeRestore:
		if req.Restore == nil {
			r.fail(fmt.Errorf("restore request without body"))
			return
		}
		// Connections from before a sleep look open but their host is gone, and
		// their terminals would count as watched forever.
		closeOthers(conn)
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
		code, err := execute(ctx, *req.Exec, stdin, r)
		if err != nil {
			r.fail(err)
			return
		}
		r.send(guestproto.Response{Type: guestproto.RespExit, ExitCode: code})
	case guestproto.TypeFiles:
		if req.Files == nil {
			r.fail(fmt.Errorf("files request without body"))
			return
		}
		if err := writeFiles(*req.Files); err != nil {
			r.fail(err)
			return
		}
		r.send(guestproto.Response{Type: guestproto.RespOK})
	default:
		r.fail(fmt.Errorf("unsupported request type %q", req.Type))
	}
}
