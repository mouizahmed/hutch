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
	"time"
	"unicode"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/rpc"
)

type Config struct {
	Dir    string // data directory: hutch.db, master.key, bucket.json
	Socket string // the API's Unix socket; group members of its directory may use it
	// Restart restarts hutchd, after a handover; Fence restarts it fenced, when
	// another machine leads. Nil in tests.
	Restart func()
	Fence   func(why string)
	// NoBoxes: this machine only coordinates, so its own machine config says
	// machine.run_boxes = false.
	NoBoxes bool
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
	started    time.Time // when this hutchd started, which no backup can predate
	gcMu       sync.Mutex
	moving     sync.Map // box ID → *sync.Mutex, held while a box moves off a lost worker
	copying    sync.Map // "<version>@<worker>" → *sync.Mutex, held while a template copies there
	// pending is memory held on each worker for boxes being placed, and starting
	// the boxes being placed or woken; guarded by mu.
	pending  map[string]int
	placing  map[string]int // boxes being placed on each worker
	starting int
	// wakes holds each box's wake in progress, which every caller waits on; parked
	// counts forwarded connections waiting for one. Guarded by mu.
	wakes  map[string]*wakeCall
	parked int
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
	// superseded is the lead record of a higher term, once one replaced this
	// machine's; leadMu orders reading and writing the record.
	superseded atomic.Pointer[link.Record]
	leadMu     sync.Mutex
	// gate stops the changing calls, and new placements, during a handover.
	gate gate
	// settingsCache holds each machine's parsed settings (cachedSettings).
	settingsCache sync.Map

	// keyProxy holds the whole keys table, to sign and refresh for workers' proxies.
	keyProxy *keys.Proxy
	// revoked are the logins you were told need hutch login; dropped, each
	// worker's last connection that ended; guarded by mu.
	revoked map[string]bool
	dropped map[string]*workerConn

	// linkWake re-checks whether the link port should listen; backupNow backs up
	// without waiting for backup.interval.
	linkWake, backupNow chan struct{}

	mu      sync.Mutex
	workers map[string]*workerConn
	// local is the worker on this machine; enrolledAt, the last enrollment
	// attempt, which limits them to one a second.
	local      string
	enrolledAt time.Time
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
	// keys is its part of the keys table; guarded by Server.mu.
	keys keys.Table
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
	s := &Server{started: time.Now(), cfg: cfg, store: st, master: master, workers: map[string]*workerConn{}, linkWake: make(chan struct{}, 1), backupNow: make(chan struct{}, 1), pending: map[string]int{}, placing: map[string]int{}, wakes: map[string]*wakeCall{}, peaks: map[string]int{}, relieving: map[string]bool{}, threadStates: map[string]map[int]api.ThreadState{},
		gitStates: map[string]guestproto.GitStatus{}, events: newHub()}
	s.keyProxy = keys.NewProxy()
	s.keyProxy.Refreshed = func(ctx context.Context, login string, changes []keys.Change) error {
		return s.keysRefreshed(ctx, api.KeysRefreshedParams{Adapter: login, Changes: changes})
	}
	s.keyProxy.Revoked = s.loginRevoked
	st.onChange = s.changed
	st.onLogin = func() {
		select {
		case s.backupNow <- struct{}{}:
		default:
		}
	}
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
	go s.linkLoop(ctx)
	go s.leadLoop(ctx)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// listenAPI serves the CLI on a Unix socket; CLIs reach it through
// `ssh <machine> hutchd api`.
func (s *Server) listenAPI(ctx context.Context) error {
	return rpc.ServeSocket(ctx, s.cfg.Socket, s.apiMux(), s.apiStreams())
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
	s.mu.Lock()
	s.local = id
	s.mu.Unlock()
	if s.cfg.NoBoxes {
		if err := s.runNoBoxes(id); err != nil {
			log.Printf("machine.run_boxes: %v", err)
		}
	}
	s.attach(ctx, id, name, conn, false)
}

// attach serves a worker over conn until it closes: the one on this machine, or
// a linked one.
func (s *Server) attach(ctx context.Context, id, name string, conn net.Conn, linked bool) {
	if err := s.store.ensureWorker(id, name); err != nil {
		log.Printf("worker %s: %v", id, err)
		conn.Close()
		return
	}
	cfg := yamuxConfig()
	if linked {
		cfg = s.linkYamux()
	}
	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		conn.Close()
		return
	}
	defer sess.Close()

	var term int64
	if l, err := s.store.link(); err == nil && l != nil {
		term = l.term
	}
	var info api.Worker
	helloCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = rpc.Call(helloCtx, sess, api.WorkerHello, api.LinkHello{Protocol: api.LinkProtocol, Term: term, WorkerID: id}, &info)
	cancel()
	if err != nil {
		log.Printf("worker %s: hello: %v", id, err)
		return
	}
	if linked && info.Link < api.LinkProtocol-1 {
		log.Printf("worker %s: hutchd %s is too old for this lead's %d link protocol: upgrade it", id, info.Version, api.LinkProtocol)
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
	info.Boxes, copies = s.reconcile(ctx, w, info.Boxes)
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
	if s.dropped == nil {
		s.dropped = map[string]*workerConn{}
	}
	s.dropped[id] = w
	s.mu.Unlock()
	s.store.workerSeen(id, info)
	s.changed()
	log.Printf("worker %s disconnected", id)
	if linked {
		s.notifyLost(id, info.Name, w)
	}
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

// workerKeys is a connected worker's part of the keys table.
func (s *Server) workerKeys(id string) keys.Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.workers[id]; w != nil {
		return w.keys
	}
	return keys.Table{}
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
		api.WorkerKeysCert: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.KeysCertParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			if !s.workerKeys(workerID).Intercepts(p.Host) {
				return nil, fmt.Errorf("%s isn't a host of this machine's logins", p.Host)
			}
			return s.keyProxy.SignCert(p.Host, p.Public)
		},
		api.WorkerKeysIdentity: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.KeysIdentityParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			if !s.workerKeys(workerID).Identifies(p.Box, p.Host) {
				return nil, fmt.Errorf("%s doesn't get an identity for %s on this machine", p.Box, p.Host)
			}
			return s.keyProxy.SignIdentity(p.Box, p.Host)
		},
		api.WorkerKeysRefresh: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var c keys.RefreshCall
			if err := rpc.Decode(params, &c); err != nil {
				return nil, err
			}
			if _, ok := s.workerKeys(workerID).Projects[c.Box]; !ok {
				return nil, fmt.Errorf("%s isn't on this machine", c.Box)
			}
			return s.keyProxy.Refresh(ctx, c)
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
				machine := workerID
				if w != nil {
					machine = w.info.Name
				}
				msg := fmt.Sprintf("%s was put to sleep: %s's memory passed machine.memory_critical; it wakes when you use it", id, machine)
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

// notifyLost tells you, once, when a worker stays unreachable for
// machines.lost_notify_after. Its boxes are never restored elsewhere without
// you: it may be cut off from this machine only, still running them.
func (s *Server) notifyLost(id, name string, gone *workerConn) {
	after := 5 * time.Minute
	if cfg, err := s.settings(); err == nil {
		after = cfg.Duration("machines.lost_notify_after")
	}
	time.AfterFunc(after, func() {
		// Only the connection that dropped counts: one made and lost since
		// starts its own wait.
		s.mu.Lock()
		back := s.workers[id] != nil || s.dropped[id] != gone
		s.mu.Unlock()
		if lost, err := s.workerLost(id); back || err != nil || lost {
			return
		}
		msg := fmt.Sprintf("%s has been unreachable for %s. If it's gone for good, hutch machine rm --lost %s brings its boxes back elsewhere from the bucket; if it's only cut off, they're still running there", name, after, name)
		s.store.event("", "unreachable", msg)
		s.notify("unreachable", "", "Machine unreachable", msg)
	})
}
