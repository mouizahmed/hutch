//go:build linux

// Package agent connects hutchd to the server and serves its calls.
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/wire"
	"github.com/mouizahmed/hutch/internal/worker"
)

// Identity is a worker's enrollment, stored in <root>/identity.
type Identity struct {
	Dir string
}

func (id Identity) path(name string) string { return filepath.Join(id.Dir, name) }

// Enroll exchanges a join token for a worker certificate. The token carries the
// server CA's fingerprint, so the worker knows it is talking to the right server
// before sending anything.
func Enroll(ctx context.Context, id Identity, serverAddr, token, name string) (string, error) {
	fp, secret, ok := strings.Cut(token, ".")
	if !ok || len(fp) != 64 {
		return "", errors.New("malformed join token")
	}
	if _, err := os.Stat(id.path("cert.pem")); err == nil {
		return "", fmt.Errorf("already enrolled (%s); remove it to enroll again", id.Dir)
	}
	if err := os.MkdirAll(id.Dir, 0o700); err != nil {
		return "", err
	}
	newKey := id.path("key.pem.new")
	defer os.Remove(newKey)
	csr, err := pki.NewWorkerKey(newKey, name)
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(serverAddr)
	if err != nil {
		return "", err
	}
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // verified below against the pinned CA
		VerifyConnection: func(cs tls.ConnectionState) error {
			certs := cs.PeerCertificates
			if len(certs) < 2 {
				return errors.New("server did not send its CA certificate")
			}
			ca := certs[len(certs)-1]
			if pki.Fingerprint(ca) != fp {
				return errors.New("server CA does not match the join token")
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: host})
			return err
		},
	}
	var d tls.Dialer
	d.Config = conf
	conn, err := d.DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := wire.Write(conn, api.EnrollRequest{Secret: secret, Name: name, CSR: csr}); err != nil {
		return "", err
	}
	var resp api.EnrollResponse
	if err := wire.Read(conn, &resp); err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	for file, data := range map[string][]byte{"ca.pem": resp.CA, "server": []byte(serverAddr + "\n")} {
		if err := os.WriteFile(id.path(file), data, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.Rename(newKey, id.path("key.pem")); err != nil {
		return "", err
	}
	return resp.WorkerID, os.WriteFile(id.path("cert.pem"), resp.Cert, 0o600)
}

// Run keeps a connection to the server open and serves its calls, reconnecting
// with backoff. Boxes keep running while the server is unreachable.
func Run(ctx context.Context, id Identity, w *worker.Worker, name string, idle IdleConfig) error {
	conf, addr, err := id.tlsConfig()
	if err != nil {
		return fmt.Errorf("not enrolled (run `hutchd enroll`): %w", err)
	}
	mux, streams := handlers(w, name), streamHandlers(w)
	var server atomic.Pointer[yamux.Session]
	go monitor(ctx, w, idle, &server)
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := serveOnce(ctx, conf, addr, mux, streams, &server)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("server connection: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil
}

func (id Identity) tlsConfig() (*tls.Config, string, error) {
	cert, err := tls.LoadX509KeyPair(id.path("cert.pem"), id.path("key.pem"))
	if err != nil {
		return nil, "", err
	}
	caPEM, err := os.ReadFile(id.path("ca.pem"))
	if err != nil {
		return nil, "", err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, "", errors.New("invalid ca.pem")
	}
	addr, err := os.ReadFile(id.path("server"))
	if err != nil {
		return nil, "", err
	}
	server := strings.TrimSpace(string(addr))
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS13}, server, nil
}

func serveOnce(ctx context.Context, conf *tls.Config, addr string, mux rpc.Mux, streams rpc.Streams, server *atomic.Pointer[yamux.Session]) error {
	d := tls.Dialer{Config: conf, NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(log.Writer(), "yamux: ", log.LstdFlags)
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()
	context.AfterFunc(ctx, func() { sess.Close() })
	server.Store(sess)
	defer server.Store(nil)
	log.Printf("connected to server %s", addr)
	if err := rpc.Serve(ctx, sess, mux, streams); err != nil {
		return err
	}
	return errors.New("connection closed")
}

func toAPI(w *worker.Worker, b *worker.Box) api.Box {
	state := string(b.State)
	if b.State == worker.Running && !w.Alive(b) {
		state = "crashed"
	}
	return api.Box{ID: b.ID, State: state, Image: b.Image, VCPUs: b.VCPUs, MemMiB: b.MemMiB,
		Generation: b.Generation, Class: b.Class, ReachIP: b.ReachIP(), Created: b.Created}
}

func handlers(w *worker.Worker, name string) rpc.Mux {
	box := func(f func(context.Context, json.RawMessage) (*worker.Box, error)) rpc.Handler {
		return func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			b, err := f(ctx, params)
			if err != nil {
				return nil, err
			}
			return toAPI(w, b), nil
		}
	}
	byID := func(op func(context.Context, string) (*worker.Box, error)) rpc.Handler {
		return box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return op(ctx, p.ID)
		})
	}
	return rpc.Mux{
		api.WorkerHello: func(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
			boxes, err := w.List()
			if err != nil {
				return nil, err
			}
			info := api.Worker{Name: name, Class: w.Class(), Version: version.String()}
			for _, b := range boxes {
				info.Boxes = append(info.Boxes, toAPI(w, b))
			}
			return info, nil
		},
		api.BoxBoot: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.BootParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Boot(ctx, p.ID, p.Image, p.VCPUs, p.MemMiB)
		}),
		api.BoxFork: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.ForkParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Fork(ctx, p.Source, p.ID)
		}),
		api.BoxSleep: byID(w.Sleep),
		api.BoxWake:  byID(w.Wake),
		api.BoxRm: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return nil, w.Remove(p.ID)
		},
		api.BoxConsole: func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ConsoleParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return rpc.ExitCode(0), streamConsole(ctx, w.ConsolePath(p.ID), p.Follow, out)
		},
		api.BoxExec: func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ExecParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			stdout, stderr := out.Writers()
			code, err := w.Exec(ctx, p.ID, guestproto.Exec{Argv: p.Argv, User: p.User}, stdout, stderr)
			if err != nil {
				return nil, err
			}
			return rpc.ExitCode(code), nil
		},
	}
}

func streamHandlers(w *worker.Worker) rpc.Streams {
	return rpc.Streams{
		// The terminal stream passes straight through to hutch-guest, which speaks
		// the PTY frames with the CLI end to end.
		api.BoxPTY: func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			var p api.PTYParams
			if err := rpc.Decode(params, &p); err != nil {
				return err
			}
			g, err := w.DialGuest(ctx, p.ID)
			if err != nil {
				return err
			}
			req := guestproto.Request{Type: guestproto.TypePTY, Version: guestproto.Version,
				PTY: &guestproto.PTY{Argv: p.Argv, User: p.User, Term: p.Term, Rows: p.Rows, Cols: p.Cols}}
			if err := wire.Write(g, req); err != nil {
				g.Close()
				return err
			}
			rpc.Splice(conn, g)
			return nil
		},
	}
}

func streamConsole(ctx context.Context, path string, follow bool, out *rpc.Out) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := out.Stdout(buf[:n]); err != nil {
				return err
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
