package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

// The worker link: other machines' hutchd dial the lead over mutual TLS. The CA
// is made by the first hutch machine add and kept in the database, so it's in
// every backup.

type linkState struct {
	ca      *pki.CA
	address string
	term    int64
}

func (s *store) link() (*linkState, error) {
	var certPEM, sealed []byte
	var l linkState
	err := s.db.QueryRow(`SELECT ca_cert, ca_key, address, term FROM link WHERE id = 1`).Scan(&certPEM, &sealed, &l.address, &l.term)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keyPEM, err := s.userKey.Open("link/ca-key", sealed)
	if err != nil {
		return nil, err
	}
	if l.ca, err = pki.ParseCA(certPEM, keyPEM); err != nil {
		return nil, err
	}
	return &l, nil
}

// ensureLink returns the link's state, making the CA on first use and recording
// the address workers dial.
func (s *store) ensureLink(address string) (*linkState, error) {
	l, err := s.link()
	if err != nil {
		return nil, err
	}
	if l == nil {
		ca, keyPEM, err := pki.NewCA()
		if err != nil {
			return nil, err
		}
		if _, err := s.exec(`INSERT INTO link (id, ca_cert, ca_key, address, term) VALUES (1, ?, ?, ?, 1)`,
			ca.PEM, s.userKey.Seal("link/ca-key", keyPEM), address); err != nil {
			return nil, err
		}
		return s.link()
	}
	if l.address != address {
		if _, err := s.exec(`UPDATE link SET address = ? WHERE id = 1`, address); err != nil {
			return nil, err
		}
		l.address = address
	}
	return l, nil
}

func (s *store) addToken(secret string, expires time.Time) error {
	_, err := s.exec(`INSERT INTO link_tokens (hash, expires) VALUES (?, ?)`, hashSecret(secret), expires.UTC())
	return err
}

// useToken marks a token used; each enrolls one worker, before it expires.
func (s *store) useToken(secret string) error {
	res, err := s.exec(`UPDATE link_tokens SET used = ? WHERE hash = ? AND used IS NULL AND expires > ?`,
		time.Now().UTC(), hashSecret(secret), time.Now().UTC())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("invalid, expired, or already used token: run hutch machine add again")
	}
	return nil
}

// enrollWorker records a new linked worker, refusing a name another machine has.
func (s *store) enrollWorker(id, name string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM workers WHERE name = ? AND gone = 0`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("a machine named %s is already added: choose another with --name", name)
	}
	now := time.Now().UTC()
	_, err := s.exec(`INSERT INTO workers (id, name, created, last_seen, linked) VALUES (?, ?, ?, ?, 1)`, id, name, now, now)
	return err
}

// linkedWorker is an enrolled worker's name; ok is false once it was removed.
func (s *store) linkedWorker(id string) (name string, ok bool) {
	err := s.db.QueryRow(`SELECT name FROM workers WHERE id = ? AND linked = 1 AND gone = 0`, id).Scan(&name)
	return name, err == nil
}

// wantLink reports whether the link port should listen: there are workers, or
// one is about to enroll.
func (s *store) wantLink() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM workers WHERE linked = 1 AND gone = 0)
		+ (SELECT count(*) FROM link_tokens WHERE used IS NULL AND expires > ?)`, time.Now().UTC()).Scan(&n)
	return n > 0, err
}

func (s *Server) machineToken(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.MachineTokenParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if s.vault.Load() == nil {
		return nil, errors.New("several machines need a bucket: add one with hutch machine bucket")
	}
	c, err := s.settings()
	if err != nil {
		return nil, err
	}
	addr := c.String("machine.address")
	if addr == "" {
		addr = p.Address
	}
	if addr == "" {
		return nil, errors.New("other machines can't know this one's address: hutch config set --machine machine.address <host>")
	}
	l, err := s.store.ensureLink(addr)
	if err != nil {
		return nil, err
	}
	secret := randomHex(24)
	if err := s.store.addToken(secret, time.Now().Add(c.Duration("machines.enroll_ttl"))); err != nil {
		return nil, err
	}
	s.wakeLink()
	return api.MachineTokenResult{Token: pki.Fingerprint(l.ca.Cert) + "." + secret,
		Lead: net.JoinHostPort(addr, strconv.FormatInt(c.Int("machine.link_port"), 10))}, nil
}

func (s *Server) wakeLink() {
	select {
	case s.linkWake <- struct{}{}:
	default:
	}
}

// linkLoop keeps the link port listening while there are workers or tokens, and
// closed otherwise, so a single machine never opens it.
func (s *Server) linkLoop(ctx context.Context) {
	var stop context.CancelFunc
	for {
		want, err := s.store.wantLink()
		if err != nil {
			log.Printf("link: %v", err)
		}
		switch {
		case want && stop == nil:
			lctx, cancel := context.WithCancel(ctx)
			if _, err := s.listenLink(lctx, s.linkAddr()); err != nil {
				log.Printf("link: %v", err)
				cancel()
			} else {
				stop = cancel
			}
		case !want && stop != nil:
			stop()
			stop = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-s.linkWake:
		case <-time.After(time.Minute):
		}
	}
}

func (s *Server) linkAddr() string {
	c, err := s.settings()
	if err != nil {
		return ""
	}
	return ":" + strconv.FormatInt(c.Int("machine.link_port"), 10)
}

func (s *Server) listenLink(ctx context.Context, addr string) (net.Addr, error) {
	l, err := s.store.link()
	if err != nil || l == nil {
		return nil, fmt.Errorf("no link CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(l.ca.Cert)
	var mu sync.Mutex
	var cert *tls.Certificate
	var certFor string
	conf := &tls.Config{
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
		// Issued for the address workers dial, again if it changes.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			l, err := s.store.link()
			if err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			if cert == nil || certFor != l.address {
				c, err := l.ca.ServerCert([]string{l.address})
				if err != nil {
					return nil, err
				}
				cert, certFor = &c, l.address
			}
			return cert, nil
		},
	}
	ln, err := tls.Listen("tcp", addr, conf)
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	log.Printf("link: listening on %s", addr)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("link: %v", err)
				}
				return
			}
			go s.handleLink(ctx, conn.(*tls.Conn))
		}
	}()
	return ln.Addr(), nil
}

func (s *Server) handleLink(ctx context.Context, conn *tls.Conn) {
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return
	}
	peers := conn.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		s.enroll(conn)
		return
	}
	conn.SetDeadline(time.Time{})
	id := peers[0].Subject.CommonName
	// Workers take a lead's hello only at a term it has claimed and still holds.
	if l, err := s.store.link(); s.superseded.Load() != nil || err != nil || l == nil || l.term == 0 {
		log.Printf("link: refused %s from %s: this machine doesn't lead at a term", id, conn.RemoteAddr())
		conn.Close()
		return
	}
	name, ok := s.store.linkedWorker(id)
	if !ok {
		log.Printf("link: refused %s from %s: removed", id, conn.RemoteAddr())
		conn.Close()
		return
	}
	s.attach(ctx, id, name, conn, true)
}

func (s *Server) enroll(conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	limited := time.Since(s.enrolledAt) < time.Second
	if !limited {
		s.enrolledAt = time.Now()
	}
	s.mu.Unlock()
	if limited {
		return
	}
	var req link.EnrollRequest
	if err := wire.Read(conn, &req); err != nil {
		return
	}
	resp := func() link.EnrollResponse {
		if req.Name == "" {
			return link.EnrollResponse{Error: "a machine needs a name"}
		}
		l, err := s.store.link()
		if err != nil || l == nil {
			return link.EnrollResponse{Error: "the lead has no link CA"}
		}
		if err := s.store.useToken(req.Secret); err != nil {
			return link.EnrollResponse{Error: err.Error()}
		}
		id := "w-" + randomHex(6)
		cert, err := l.ca.SignWorker(req.CSR, id)
		if err != nil {
			return link.EnrollResponse{Error: err.Error()}
		}
		if err := s.store.enrollWorker(id, req.Name); err != nil {
			return link.EnrollResponse{Error: err.Error()}
		}
		s.store.event("", "machine added", req.Name)
		log.Printf("link: enrolled %s (%s)", id, req.Name)
		return link.EnrollResponse{WorkerID: id, Cert: cert, CA: l.ca.PEM}
	}()
	wire.Write(conn, resp)
}

// linkYamux is yamux for a worker link: a worker silent past
// machines.unreachable_after is dropped.
func (s *Server) linkYamux() *yamux.Config {
	c := yamuxConfig()
	if cfg, err := s.settings(); err == nil {
		c.KeepAliveInterval = cfg.Duration("machines.heartbeat")
		c.ConnectionWriteTimeout = max(cfg.Duration("machines.unreachable_after")-c.KeepAliveInterval, time.Second)
	}
	return c
}
