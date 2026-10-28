package guestproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/wire"
)

// Client talks to hutch-guest through Firecracker's vsock Unix socket, or over TCP
// with a token in gVisor boxes, which have no vsock.
type Client struct {
	UDSPath string
	Addr    string
	Token   string
}

// Dial opens a raw connection to hutch-guest, retrying failed handshakes for up
// to a second (for example while hutch-guest restarts); no request has been sent
// yet, so a retry is always safe.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := c.dialOnce(ctx)
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *Client) dialOnce(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	network, addr, hello := "unix", c.UDSPath, fmt.Sprintf("CONNECT %d\n", Port)
	if c.Addr != "" {
		network, addr, hello = "tcp", c.Addr, "TOKEN "+c.Token+"\n"
	}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := io.WriteString(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := ReadLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("guest handshake: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("guest handshake: %q", line)
	}
	return conn, nil
}

// ReadLine reads byte by byte so nothing after the handshake line is consumed.
func ReadLine(r io.Reader) (string, error) {
	var b strings.Builder
	var c [1]byte
	for b.Len() < 256 {
		if _, err := r.Read(c[:]); err != nil {
			return "", err
		}
		if c[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c[0])
	}
	return "", errors.New("handshake line too long")
}

func (c *Client) call(ctx context.Context, req Request, handle func(Response) (done bool, err error)) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req.Version = Version
	if err := wire.Write(conn, req); err != nil {
		return err
	}
	for {
		var resp Response
		if err := wire.Read(conn, &resp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if resp.Type == RespError {
			return fmt.Errorf("guest: %s", resp.Error)
		}
		done, err := handle(resp)
		if err != nil || done {
			return err
		}
	}
}

func (c *Client) Hello(ctx context.Context) (Response, error) {
	var hello Response
	err := c.call(ctx, Request{Type: TypeHello}, func(r Response) (bool, error) {
		hello = r
		return true, nil
	})
	return hello, err
}

func (c *Client) Restore(ctx context.Context, r Restore) error {
	return c.call(ctx, Request{Type: TypeRestore, Restore: &r}, func(Response) (bool, error) {
		return true, nil
	})
}

func (c *Client) Exec(ctx context.Context, e Exec, stdout, stderr io.Writer) (int, error) {
	code := -1
	err := c.call(ctx, Request{Type: TypeExec, Exec: &e}, func(r Response) (bool, error) {
		switch r.Type {
		case RespStdout:
			_, err := stdout.Write(r.Data)
			return false, err
		case RespStderr:
			_, err := stderr.Write(r.Data)
			return false, err
		case RespExit:
			code = r.ExitCode
			return true, nil
		}
		return false, fmt.Errorf("unexpected response %q", r.Type)
	})
	return code, err
}

func (c *Client) Status(ctx context.Context, watch ...string) (Status, error) {
	var st Status
	req := Request{Type: TypeStatus}
	if len(watch) > 0 {
		req.Status = &StatusParams{Watch: watch}
	}
	err := c.call(ctx, req, func(r Response) (bool, error) {
		if r.Status != nil {
			st = *r.Status
		}
		return true, nil
	})
	return st, err
}

func (c *Client) Files(ctx context.Context, f Files) error {
	return c.call(ctx, Request{Type: TypeFiles, Files: &f}, func(Response) (bool, error) {
		return true, nil
	})
}

// DialPort opens a byte stream to a TCP port on the guest's loopback.
func (c *Client) DialPort(ctx context.Context, port int) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := wire.Write(conn, Request{Type: TypeDial, Version: Version, Dial: &Dial{Port: port}}); err != nil {
		conn.Close()
		return nil, err
	}
	var r Response
	if err := wire.Read(conn, &r); err != nil {
		conn.Close()
		return nil, err
	}
	if r.Type == RespError {
		conn.Close()
		return nil, errors.New(r.Error)
	}
	return conn, nil
}
