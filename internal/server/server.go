package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

type Config struct {
	Dir    string // data directory: hutch.db, master.key, bucket.json
	Socket string // the API's Unix socket; group members of its directory may use it
}

type Server struct {
	cfg   Config
	store *store
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
	// peaks holds each awake box's peak memory use as last reported, and relieving
	// the workers a box is being moved off for memory; guarded by mu.
	peaks     map[string]int
	relieving map[string]bool
	// Bucket requests not yet in the ledger, and the server's own count when last
	// read; guarded by mu. bucketBytes is the bucket's size, measured hourly.
	requests, ownRequests [2]int64
	bucketBytes           atomic.Int64

	// keysMu orders changes to the keys proxy's state and the pushes of its table.
	keysMu sync.Mutex

	mu      sync.Mutex
	workers map[string]*workerConn
	// threadStates holds the latest reported state of each thread, by box and number.
	threadStates map[string]map[int]api.ThreadState
	// gitStates holds each box's latest git status.
	gitStates map[string]guestproto.GitStatus
	events    *hub
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
	s := &Server{cfg: cfg, store: st, master: master, workers: map[string]*workerConn{}, pending: map[string]int{}, peaks: map[string]int{}, relieving: map[string]bool{}, threadStates: map[string]map[int]api.ThreadState{},
		gitStates: map[string]guestproto.GitStatus{}, events: newHub()}
	st.onChange = s.changed
	go s.publish()
	s.changed()
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
	return s, nil
}

func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.listenAPI(ctx) }()
	if s.vault.Load() == nil {
		log.Printf("no bucket: nothing is backed up until hutch machine bucket")
	}
	go s.backupLoop(ctx)
	go s.gcLoop(ctx)
	go s.archiveLoop(ctx)
	go s.maxAwakeLoop(ctx)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// listenAPI serves the CLI on a Unix socket open to hutchd's user and its
// directory's group; CLIs reach it through `ssh <machine> hutchd api`.
func (s *Server) listenAPI(ctx context.Context) error {
	path := s.cfg.Socket
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Dir(path)); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(path, -1, int(st.Gid))
		}
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

// AttachWorker serves the worker on this machine over conn, the other end of
// which its agent serves, until conn closes.
func (s *Server) AttachWorker(ctx context.Context, id, name string, conn net.Conn) {
	if err := s.store.ensureWorker(id, name); err != nil {
		log.Printf("worker %s: %v", id, err)
		conn.Close()
		return
	}
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
	s.pushKeys(ctx, w)
	s.changed()
	log.Printf("worker %s (%s) connected: %s, %d boxes", id, info.Name, info.Class, len(info.Boxes))
	go s.pruneAll(ctx)

	<-sess.CloseChan()
	s.mu.Lock()
	if s.workers[id] == w {
		delete(s.workers, id)
	}
	s.mu.Unlock()
	s.store.workerSeen(id, info)
	s.changed()
	log.Printf("worker %s disconnected", id)
}

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
		api.WorkerKeysRefreshed: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.KeysRefreshedParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return nil, s.keysRefreshed(ctx, p)
		},
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
				if r.Pressure == "high" {
					go s.relieve(w)
				}
			}
			for _, c := range r.Crashed {
				go s.recoverCrash(workerID, c)
			}
			for _, id := range r.ForceSlept {
				msg := fmt.Sprintf("%s was put to sleep: the machine's memory passed machine.memory_critical; it wakes when you use it", id)
				s.store.event(id, "force-slept", msg)
				s.notify("force-slept", id, "Box put to sleep", msg)
			}
			for _, br := range r.Boxes {
				prev, err := s.store.box(br.Box.ID)
				if err == nil && prev.Worker != workerID {
					continue // moved off this worker while it was gone; removed when it reconnects
				}
				if err != nil && s.store.removed(br.Box.ID) {
					continue // a report sent before the box was removed
				}
				if err == nil && br.Box.Generation < prev.Generation {
					continue // a report sent before a snapshot the server already has, like a sleep's
				}
				br.Box.Worker = workerID
				if err := s.store.putBox(br.Box); err != nil {
					return nil, err
				}
				if err == nil && prev.State != br.Box.State {
					s.store.event(br.Box.ID, "state", br.Box.State)
				}
				s.mu.Lock()
				alerts := s.applyThreads(br.Box.ID, br.Box.State == "running", br.Threads)
				if g := br.Git; g != nil {
					s.gitStates[br.Box.ID] = *g
					if g.Branch != "" && prev.Branch != "" && g.Branch != prev.Branch {
						s.store.setBoxGit(br.Box.ID, g.Branch, prev.Base)
					}
				}
				s.mu.Unlock()
				for _, t := range alerts {
					s.alert(t)
				}
			}
			s.changed()
			return nil, nil
		},
		api.WorkerThreads: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.WorkerThreadsParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			if b, err := s.store.box(p.Box); err != nil || b.Worker != workerID || b.State != "running" {
				return nil, nil
			}
			s.mu.Lock()
			alerts := s.applyThreads(p.Box, true, p.Threads)
			s.mu.Unlock()
			for _, t := range alerts {
				s.alert(t)
			}
			s.changed()
			return nil, nil
		},
	}
}

// applyThreads takes a box's reported threads, with s.mu held: their states, and
// whether each is unread, cached with the box. A box that stops keeps its threads
// listed, and forgets their states, so the first report after a wake, whose redraw
// is output, is only a baseline. It returns the agents that want you: a new bell,
// or an exit, while no one looked. Output alone never alerts: an idle app redraws.
func (s *Server) applyThreads(box string, running bool, threads []api.ThreadState) (alerts []api.Thread) {
	states := map[int]api.ThreadState{}
	for _, t := range threads {
		states[t.N] = t
	}
	prevStates := s.threadStates[box]
	if !running {
		delete(s.threadStates, box)
		return nil
	}
	s.threadStates[box] = states
	b, err := s.store.box(box)
	if err != nil {
		return nil
	}
	unread := map[int]bool{}
	for _, t := range b.Threads {
		unread[t.N] = t.Unread
	}
	var out []api.Thread
	for _, t := range threads {
		// A thread that worked and stopped while nobody looked is unread, even
		// when the turn fell between two reports; looking at it, or it working
		// again, reads it.
		u := unread[t.N]
		prev, known := prevStates[t.N]
		switch {
		case t.Viewed || t.State == api.ThreadWorking:
			u = false
		case known && (prev.State == api.ThreadWorking || t.LastOutput.After(prev.LastOutput)):
			u = true
		}
		th := api.Thread{Box: box, N: t.N, Agent: t.Agent, Autonomy: t.Autonomy, Title: threadTitle(b, t),
			Created: t.Created, State: t.State, Unread: u}
		out = append(out, th)
		if known && t.Agent != "" && !t.Viewed &&
			(t.Bell.After(prev.Bell) || t.State == api.ThreadExited && prev.State != api.ThreadExited) {
			alerts = append(alerts, th)
		}
	}
	s.store.setThreads(box, out)
	return alerts
}

// threadTitle is your name for a thread, else the title its program gives its
// terminal (without leading status symbols, a trailing " | <folder>" or
// " - <folder>" for the box's folder, and not the bare hostname), else the first
// line of what it was started with.
func threadTitle(b api.Box, t api.ThreadState) string {
	if t.Name != "" {
		return t.Name
	}
	title := strings.TrimLeftFunc(t.Title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	dir := b.Path
	if dir == "" {
		dir = workspace
	}
	for _, sep := range []string{" | ", " - "} {
		title = strings.TrimSuffix(title, sep+filepath.Base(dir))
	}
	if title == "" || title == b.ID || strings.HasPrefix(title, b.ID+".") || title == filepath.Base(dir) {
		title = t.Prompt
	}
	if r := []rune(title); len(r) > 80 {
		title = string(r[:79]) + "…"
	}
	return title
}
