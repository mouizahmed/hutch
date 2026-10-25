// Package rpc carries calls over multiplexed streams: each call opens a stream,
// sends one Request, and reads Frames until a result, error, or exit frame.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/wire"
)

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

const (
	FrameResult = "result"
	FrameError  = "error"
	FrameStdout = "stdout"
	FrameStderr = "stderr"
	FrameExit   = "exit"
)

type Frame struct {
	Type     string          `json:"type"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	Data     []byte          `json:"data,omitempty"`
	ExitCode int             `json:"exit_code,omitempty"`
}

// Out sends intermediate frames (output streams) from a handler.
type Out struct {
	mu   sync.Mutex
	conn net.Conn
}

func (o *Out) send(f Frame) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return wire.Write(o.conn, f)
}

func (o *Out) Stdout(b []byte) error { return o.send(Frame{Type: FrameStdout, Data: b}) }
func (o *Out) Stderr(b []byte) error { return o.send(Frame{Type: FrameStderr, Data: b}) }

// Writers adapts Out to io.Writers for stdout and stderr.
func (o *Out) Writers() (stdout, stderr io.Writer) {
	return writerFunc(o.Stdout), writerFunc(o.Stderr)
}

type writerFunc func([]byte) error

func (f writerFunc) Write(b []byte) (int, error) {
	if err := f(append([]byte(nil), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// ExitCode is returned by a handler to end the call with an exit frame.
type ExitCode int

type Handler func(ctx context.Context, params json.RawMessage, out *Out) (any, error)

type Mux map[string]Handler

// Serve accepts streams from a session and dispatches each to its handler until
// the session closes.
func Serve(ctx context.Context, sess *yamux.Session, mux Mux) error {
	for {
		conn, err := sess.Accept()
		if err != nil {
			if errors.Is(err, io.EOF) || sess.IsClosed() {
				return nil
			}
			return err
		}
		go handle(ctx, conn, mux)
	}
}

func handle(ctx context.Context, conn net.Conn, mux Mux) {
	defer conn.Close()
	var req Request
	if err := wire.Read(conn, &req); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		var b [1]byte
		conn.Read(b[:])
		cancel()
	}()

	out := &Out{conn: conn}
	h, ok := mux[req.Method]
	if !ok {
		out.send(Frame{Type: FrameError, Error: fmt.Sprintf("unknown method %q", req.Method)})
		return
	}
	result, err := h(ctx, req.Params, out)
	switch {
	case err != nil:
		out.send(Frame{Type: FrameError, Error: err.Error()})
	default:
		if code, ok := result.(ExitCode); ok {
			out.send(Frame{Type: FrameExit, ExitCode: int(code)})
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Printf("rpc %s: marshal result: %v", req.Method, err)
			out.send(Frame{Type: FrameError, Error: "internal error"})
			return
		}
		out.send(Frame{Type: FrameResult, Result: b})
	}
}

// Stream calls method and passes every frame to onFrame until the final one.
func Stream(ctx context.Context, sess *yamux.Session, method string, params any, onFrame func(Frame) error) error {
	conn, err := sess.Open()
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := wire.Write(conn, Request{Method: method, Params: p}); err != nil {
		return err
	}
	for {
		var f Frame
		if err := wire.Read(conn, &f); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s: %w", method, err)
		}
		if f.Type == FrameError {
			return errors.New(f.Error)
		}
		if err := onFrame(f); err != nil {
			return err
		}
		if f.Type == FrameResult || f.Type == FrameExit {
			return nil
		}
	}
}

// Call calls method and decodes its result into result (which may be nil).
func Call(ctx context.Context, sess *yamux.Session, method string, params, result any) error {
	return Stream(ctx, sess, method, params, func(f Frame) error {
		if f.Type == FrameResult && result != nil {
			return json.Unmarshal(f.Result, result)
		}
		return nil
	})
}

// Exec calls a method that streams output and ends with an exit code.
func Exec(ctx context.Context, sess *yamux.Session, method string, params any, stdout, stderr io.Writer) (int, error) {
	code := -1
	err := Stream(ctx, sess, method, params, func(f Frame) error {
		switch f.Type {
		case FrameStdout:
			_, err := stdout.Write(f.Data)
			return err
		case FrameStderr:
			_, err := stderr.Write(f.Data)
			return err
		case FrameExit:
			code = f.ExitCode
		}
		return nil
	})
	return code, err
}

// Decode unmarshals handler params; unknown fields are ignored for compatibility.
func Decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return errors.New("missing params")
	}
	return json.Unmarshal(params, v)
}

// Relay forwards a call from one session to another, frame by frame, so the
// server can pass CLI calls through to a worker without understanding them.
func Relay(ctx context.Context, to *yamux.Session, method string, params json.RawMessage, out *Out) (any, error) {
	var result json.RawMessage
	var code *int
	err := Stream(ctx, to, method, params, func(f Frame) error {
		switch f.Type {
		case FrameStdout, FrameStderr:
			return out.send(f)
		case FrameResult:
			result = f.Result
		case FrameExit:
			c := f.ExitCode
			code = &c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if code != nil {
		return ExitCode(*code), nil
	}
	return result, nil
}
