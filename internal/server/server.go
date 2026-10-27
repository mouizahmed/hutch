package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
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
	// master derives every key ([durable.Key]); vault is the bucket, once initialized.
	master   durable.Key
	vault    atomic.Pointer[vault]
	recorded sync.Map // projects whose ID record is in the bucket
	backupMu sync.Mutex
	// The last backup's time and the changes it covered; guarded by backupMu.
	lastBackup time.Time
	backedUp   int64
	gcMu       sync.Mutex
	moving     sync.Map // box ID → *sync.Mutex, held while a box moves off a lost worker
	copying    sync.Map // "<version>@<worker>" → *sync.Mutex, held while a template copies there
	// pending is memory held on each worker for boxes being placed, and starting
	// the boxes being placed or woken; guarded by mu.
	pending  map[string]int
	starting int
	fleet    atomic.Pointer[fleet]
	// prestarted holds workers started early that no box has landed on yet; guarded by mu.
	prestarted map[string]bool
	// missing holds when each elastic worker's instance was first missing from the
	// provider's listing; guarded by mu.
	missing map[string]time.Time
	// peaks holds each awake box's peak memory use as last reported, and relieving
	// the workers a box is being moved off for memory; guarded by mu.
	peaks     map[string]int
	relieving map[string]bool
	// Bucket requests not yet in the ledger, and the server's own count when last
	// read; guarded by mu. bucketBytes is the bucket's size, measured hourly.
	requests, ownRequests [2]int64
	bucketBytes           atomic.Int64

	mu      sync.Mutex
	workers map[string]*workerConn
	// threadStates holds the latest reported state of each thread, by box and number.
	threadStates map[string]map[int]api.ThreadState
	// fileHashes holds the last reported hashes of each box's watched credential
	// files; syncing marks boxes whose changed credentials are being read.
	fileHashes map[string]map[string]string
	syncing    map[string]bool
	// pushErrors holds each box's latest failed branch push.
	pushErrors map[string]string
	// threadWatch decides when a thread's stop is worth a notification, by box/n.
	threadWatch map[string]*threadWatch
}

type workerConn struct {
	id   string
	sess *yamux.Session
	info api.Worker
	// projects whose keys the worker was given, and boxes it has yet to upload;
	// guarded by Server.mu.
	projects  map[string]bool
	uploading int
	requests  [2]int64 // its bucket requests as last reported
	// Its memory and how much is available, in MiB, as last reported.
	memTotal, memAvailable int
}

func SocketPath(dir string) string { return filepath.Join(dir, "api.sock") }

func New(cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	master, err := loadMaster(cfg.Dir)
	if err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(cfg.Dir, "hutch.db"), master.Derive("user-layer"))
	if err != nil {
		return nil, err
	}
	ca, err := pki.LoadOrCreateCA(cfg.Dir)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, ca: ca, master: master, workers: map[string]*workerConn{}, pending: map[string]int{}, prestarted: map[string]bool{}, missing: map[string]time.Time{}, peaks: map[string]int{}, relieving: map[string]bool{}, threadStates: map[string]map[int]api.ThreadState{}, threadWatch: map[string]*threadWatch{},
		fileHashes: map[string]map[string]string{}, syncing: map[string]bool{}, pushErrors: map[string]string{}}
	b, bcfg, err := loadBucket(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if b != nil {
		if err := (&durable.Store{Bucket: b}).CheckFormat(context.Background()); err != nil {
			return nil, err
		}
		s.vault.Store(&vault{bucket: b, cfg: *bcfg})
	}
	f, err := loadFleet(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if f != nil {
		s.fleet.Store(f)
	}
	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- s.listenWorkers(ctx) }()
	go func() { errc <- s.listenAPI(ctx) }()
	if s.vault.Load() == nil {
		log.Printf("no bucket: nothing is backed up until hutch server init")
	}
	go s.backupLoop(ctx)
	go s.gcLoop(ctx)
	go s.archiveLoop(ctx)
	go s.teardownLoop(ctx)
	go s.costLoop(ctx)
	go s.bucketSizeLoop(ctx)
	go s.maxAwakeLoop(ctx)
	go s.mergeLoop(ctx)
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
	if err := s.store.workerSeen(id, info); err != nil {
		log.Printf("worker %s: %v", id, err)
		return
	}
	go rpc.Serve(ctx, sess, s.workerMux(id), nil)
	// Busy until its first report says what it has left to upload.
	w := &workerConn{id: id, sess: sess, info: info, projects: map[string]bool{}, uploading: 1}
	var copies []string
	info.Boxes, copies = s.dropStale(ctx, w, info.Boxes)
	if err := s.store.syncWorkerBoxes(id, info.Boxes, copies); err != nil {
		log.Printf("worker %s: sync boxes: %v", id, err)
	}
	for _, b := range info.Boxes {
		if m, err := s.store.meta(b.ID); err == nil && m.project != "" {
			w.projects[m.project] = true
		}
	}
	s.sendWorkerSettings(ctx, w)
	s.mu.Lock()
	if old := s.workers[id]; old != nil {
		old.sess.Close()
	}
	s.workers[id] = w
	s.mu.Unlock()
	log.Printf("worker %s (%s) connected: %s, %d boxes", id, info.Name, info.Class, len(info.Boxes))
	go s.pruneAll(ctx)

	<-sess.CloseChan()
	s.mu.Lock()
	if s.workers[id] == w {
		delete(s.workers, id)
	}
	s.mu.Unlock()
	s.store.workerSeen(id, info)
	log.Printf("worker %s disconnected", id)
}

func (s *Server) enroll(conn *tls.Conn) {
	defer conn.Close()
	var req api.EnrollRequest
	if err := wire.Read(conn, &req); err != nil {
		return
	}
	resp := func() api.EnrollResponse {
		node, err := s.store.useToken(req.Secret)
		if err != nil {
			return api.EnrollResponse{Error: err.Error()}
		}
		id := "w-" + randomHex(6)
		cert, err := s.ca.SignWorker(req.CSR, id)
		if err != nil {
			return api.EnrollResponse{Error: err.Error()}
		}
		if err := s.store.addWorker(id, req.Name, node); err != nil {
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
	if err := s.store.addToken(secret, ""); err != nil {
		return "", err
	}
	return pki.Fingerprint(s.ca.Cert) + "." + secret, nil
}

func fingerprint(s *Server) string { return pki.Fingerprint(s.ca.Cert) }

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func rpcCall(ctx context.Context, w *workerConn, method string, params any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return rpc.Call(ctx, w.sess, method, params, nil)
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

// workerMux serves calls a worker makes to the server.
func (s *Server) workerMux(workerID string) rpc.Mux {
	return rpc.Mux{
		api.WorkerReport: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var r api.Report
			if err := rpc.Decode(params, &r); err != nil {
				return nil, err
			}
			s.mu.Lock()
			w := s.workers[workerID]
			if w != nil {
				if r.Disk.Total > 0 {
					w.info.Disk = r.Disk
				}
				w.uploading = r.Uploading
				w.memTotal, w.memAvailable = r.MemTotal, r.MemAvailable
			}
			for _, br := range r.Boxes {
				if br.PeakMiB > 0 && br.Box.State == "running" {
					s.peaks[br.Box.ID] = br.PeakMiB
				} else {
					delete(s.peaks, br.Box.ID)
				}
			}
			s.mu.Unlock()
			if w != nil {
				s.countRequests(w, r.Requests)
				if r.Pressure == "high" {
					go s.relieve(w)
				}
			}
			for _, c := range r.Crashed {
				go s.recoverCrash(workerID, c)
			}
			for _, id := range r.ForceSlept {
				msg := fmt.Sprintf("%s was put to sleep: worker %s's memory passed worker.memory_critical; it wakes when you use it", id, workerID)
				s.store.event(id, "force-slept", msg)
				s.notify("force-slept", id, "Box put to sleep", msg)
			}
			for _, br := range r.Boxes {
				prev, err := s.store.box(br.Box.ID)
				if err == nil && prev.Worker != workerID {
					continue // moved off this worker while it was gone; removed when it reconnects
				}
				br.Box.Worker = workerID
				if err := s.store.putBox(br.Box); err != nil {
					return nil, err
				}
				if err == nil && prev.State != br.Box.State {
					s.store.event(br.Box.ID, "state", br.Box.State)
				}
				states := map[int]api.ThreadState{}
				for _, t := range br.Threads {
					states[t.N] = t
				}
				s.mu.Lock()
				prevStates := s.threadStates[br.Box.ID]
				// A box put to sleep reports no threads; keep their last titles and tokens.
				if len(states) > 0 || br.Box.State == "running" {
					s.threadStates[br.Box.ID] = states
				}
				for n, t := range states {
					if t.SessionID != "" && prevStates[n].SessionID != t.SessionID {
						s.store.updateThread(br.Box.ID, n, "session_id", t.SessionID)
					}
				}
				if br.Push != nil {
					if prev := s.pushErrors[br.Box.ID]; br.Push.Error != prev && br.Push.Error != "" {
						s.store.event(br.Box.ID, "push", br.Push.Error)
					}
					s.pushErrors[br.Box.ID] = br.Push.Error
				}
				s.mu.Unlock()
				s.watchThreads(br.Box.ID, states)
				s.watchReport(br)
			}
			return nil, nil
		},
	}
}
