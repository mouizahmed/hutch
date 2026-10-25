package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

type Config struct {
	Dir    string   // data directory: hutch.db, CA, api.sock
	Listen string   // worker address, e.g. ":7443"
	Names  []string // names and IPs workers use to reach the server
}

type Server struct {
	cfg   Config
	store *store
	ca    *pki.CA

	mu      sync.Mutex
	workers map[string]*workerConn
	// threadStates holds the latest reported state of each thread, by box and number.
	threadStates map[string]map[int]string
}

type workerConn struct {
	id   string
	sess *yamux.Session
	info api.Worker
}

func SocketPath(dir string) string { return filepath.Join(dir, "api.sock") }

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(cfg.Dir, "hutch.db"))
	if err != nil {
		return nil, err
	}
	ca, err := pki.LoadOrCreateCA(cfg.Dir)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, ca: ca, workers: map[string]*workerConn{}, threadStates: map[string]map[int]string{}}, nil
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- s.listenWorkers(ctx) }()
	go func() { errc <- s.listenAPI(ctx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// listenAPI serves the CLI on a Unix socket only the server's user can open;
// remote CLIs reach it through `ssh <server> hutch api`.
func (s *Server) listenAPI(ctx context.Context) error {
	path := SocketPath(s.cfg.Dir)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	mux := s.apiMux()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			sess, err := yamux.Server(conn, yamuxConfig())
			if err != nil {
				conn.Close()
				return
			}
			defer sess.Close()
			rpc.Serve(ctx, sess, mux, s.apiStreams())
		}()
	}
}

func yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = nil
	c.Logger = log.New(log.Writer(), "yamux: ", log.LstdFlags)
	return c
}

func (s *Server) listenWorkers(ctx context.Context) error {
	cert, err := s.ca.ServerCert(s.cfg.Names)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.Cert)
	ln, err := tls.Listen("tcp", s.cfg.Listen, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		return err
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	log.Printf("listening for workers on %s", s.cfg.Listen)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleWorkerConn(ctx, conn.(*tls.Conn))
	}
}

func (s *Server) handleWorkerConn(ctx context.Context, conn *tls.Conn) {
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
	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		conn.Close()
		return
	}
	defer sess.Close()

	var info api.Worker
	helloCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = rpc.Call(helloCtx, sess, api.WorkerHello, struct{}{}, &info)
	cancel()
	if err != nil {
		log.Printf("worker %s: hello: %v", id, err)
		return
	}
	info.ID, info.Online = id, true
	if err := s.store.workerSeen(id, info.Class, info.Version); err != nil {
		log.Printf("worker %s: %v", id, err)
		return
	}
	if err := s.store.syncWorkerBoxes(id, info.Boxes); err != nil {
		log.Printf("worker %s: sync boxes: %v", id, err)
	}
	go rpc.Serve(ctx, sess, s.workerMux(id), nil)
	w := &workerConn{id: id, sess: sess, info: info}
	s.sendWorkerSettings(ctx, w)
	s.mu.Lock()
	if old := s.workers[id]; old != nil {
		old.sess.Close()
	}
	s.workers[id] = w
	s.mu.Unlock()
	log.Printf("worker %s (%s) connected: %s, %d boxes", id, info.Name, info.Class, len(info.Boxes))

	<-sess.CloseChan()
	s.mu.Lock()
	if s.workers[id] == w {
		delete(s.workers, id)
	}
	s.mu.Unlock()
	s.store.workerSeen(id, info.Class, info.Version)
	log.Printf("worker %s disconnected", id)
}

func (s *Server) enroll(conn *tls.Conn) {
	defer conn.Close()
	var req api.EnrollRequest
	if err := wire.Read(conn, &req); err != nil {
		return
	}
	resp := func() api.EnrollResponse {
		if err := s.store.useToken(req.Secret); err != nil {
			return api.EnrollResponse{Error: err.Error()}
		}
		id := "w-" + randomHex(6)
		cert, err := s.ca.SignWorker(req.CSR, id)
		if err != nil {
			return api.EnrollResponse{Error: err.Error()}
		}
		if err := s.store.addWorker(id, req.Name); err != nil {
			return api.EnrollResponse{Error: err.Error()}
		}
		log.Printf("enrolled worker %s (%s)", id, req.Name)
		return api.EnrollResponse{WorkerID: id, Cert: cert, CA: s.ca.PEM}
	}()
	wire.Write(conn, resp)
}

// NewToken creates a single-use join token: <CA fingerprint>.<secret>.
func (s *Server) NewToken() (string, error) {
	secret := randomHex(24)
	if err := s.store.addToken(secret); err != nil {
		return "", err
	}
	return pki.Fingerprint(s.ca.Cert) + "." + secret, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) worker(id string) (*workerConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workers[id]
	if w == nil {
		return nil, fmt.Errorf("worker %s is offline", id)
	}
	return w, nil
}

// placement picks the worker for a new box. Phase 1: the requested worker, or the
// only connected one.
func (s *Server) placement(requested string) (*workerConn, error) {
	if requested != "" {
		return s.worker(requested)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		return w, nil
	}
	return nil, errors.New("no workers online")
}

// workerMux serves calls a worker makes to the server.
func (s *Server) workerMux(workerID string) rpc.Mux {
	return rpc.Mux{
		api.WorkerReport: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var r api.Report
			if err := rpc.Decode(params, &r); err != nil {
				return nil, err
			}
			for _, br := range r.Boxes {
				prev, err := s.store.box(br.Box.ID)
				br.Box.Worker = workerID
				if err := s.store.putBox(br.Box); err != nil {
					return nil, err
				}
				if err == nil && prev.State != br.Box.State {
					s.store.event(br.Box.ID, "state", br.Box.State)
				}
				states := map[int]string{}
				for _, t := range br.Threads {
					states[t.N] = t.State
				}
				s.mu.Lock()
				s.threadStates[br.Box.ID] = states
				s.mu.Unlock()
			}
			return nil, nil
		},
	}
}
