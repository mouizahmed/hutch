package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// fakeWorker answers the lead's calls over a dialed link until it closes.
func fakeWorker(t *testing.T, conn net.Conn, name string) *yamux.Session {
	sess, err := yamux.Server(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ok := func(context.Context, json.RawMessage, *rpc.Out) (any, error) { return nil, nil }
	go rpc.Serve(context.Background(), sess, rpc.Mux{
		api.WorkerHello: func(context.Context, json.RawMessage, *rpc.Out) (any, error) {
			return api.Worker{Name: name, Link: api.LinkProtocol}, nil
		},
		api.WorkerSettings: ok,
		api.WorkerKeys:     ok,
	}, nil)
	return sess
}

func TestLinkEnrollAndRevoke(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.store.ensureLink("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.listenLink(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lead := addr.String()
	token := func(expires time.Time) string {
		secret := randomHex(24)
		if err := s.store.addToken(secret, expires, ""); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.enrolledAt = time.Time{} // past the one-a-second limit
		s.mu.Unlock()
		return pki.Fingerprint(l.ca.Cert) + "." + secret
	}
	enrollAs := func(tok, name string) (link.Dir, string, error) {
		dir := link.Dir(filepath.Join(t.TempDir(), "link"))
		id, err := link.Enroll(ctx, dir, lead, tok, name)
		return dir, id, err
	}
	enroll := func(tok string) (link.Dir, string, error) { return enrollAs(tok, "w1") }

	if _, _, err := enroll(token(time.Now().Add(-time.Minute))); err == nil {
		t.Fatal("an expired token enrolled")
	}
	other, _, _ := pki.NewCA()
	_, secret, _ := strings.Cut(token(time.Now().Add(time.Minute)), ".")
	if _, _, err := enroll(pki.Fingerprint(other.Cert) + "." + secret); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a token for another CA: %v", err)
	}
	tok := token(time.Now().Add(time.Minute))
	dir, id, err := enroll(tok)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.enrolledAt = time.Time{}
	s.mu.Unlock()
	if _, _, err := enroll(tok); err == nil {
		t.Fatal("a used token enrolled again")
	}
	// A name already taken is refused without using up the token.
	tok2 := token(time.Now().Add(time.Minute))
	if _, _, err := enroll(tok2); err == nil || !strings.Contains(err.Error(), "already added") {
		t.Fatalf("a second machine named w1: %v", err)
	}
	s.mu.Lock()
	s.enrolledAt = time.Time{}
	s.mu.Unlock()
	if _, _, err := enrollAs(tok2, "w2"); err != nil {
		t.Fatalf("the token after a refused name: %v", err)
	}

	conn, err := link.Dial(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := fakeWorker(t, conn, "w1")
	defer sess.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := s.worker(id); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the enrolled worker never attached")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := s.workerRm(ctx, mustJSON(api.IDParams{ID: id}), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sess.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("a removed worker kept its link")
	}
	conn, err = link.Dial(ctx, dir)
	if err == nil {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = conn.Read(make([]byte, 1))
		conn.Close()
	}
	if err == nil {
		t.Fatal("a removed worker connected again")
	}
}

// A worker's certificate is for clients only, so a worker can't pose as a lead.
func TestLinkWorkerCertIsNoLead(t *testing.T) {
	ca, _, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csr, err := pki.NewWorkerKey("w1")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignWorker(csr, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	dir := t.TempDir()
	for name, data := range map[string][]byte{"key.pem": keyPEM, "cert.pem": certPEM, "ca.pem": ca.PEM, "lead": []byte(ln.Addr().String())} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if conn, err := link.Dial(context.Background(), link.Dir(dir)); err == nil {
		conn.Close()
		t.Fatal("a worker's certificate was accepted as the lead's")
	}
}

func TestLinkRefusesLowerTerm(t *testing.T) {
	dir := link.Dir(t.TempDir())
	if err := link.CheckTerm(dir, 3); err != nil {
		t.Fatal(err)
	}
	if err := link.CheckTerm(dir, 2); err == nil {
		t.Fatal("a lead at a lower term was accepted")
	}
	if err := link.CheckTerm(dir, 3); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
