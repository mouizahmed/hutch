package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/pki"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

// changing are the calls that change the lead's state. A handover waits for
// those under way and refuses new ones; the rest (reads, sessions in boxes,
// sleep and wake, which workers' reports carry over) go on.
var changing = []string{api.BoxBoot, api.BoxFork, api.BoxRm, api.BoxMove, api.BoxResize, api.UserPutConfig,
	api.UserLoginBox, api.UserCapture, api.ProjectInit, api.ProjectConfirm, api.ProjectRm, api.TemplateBuild,
	api.TemplateSnapshot, api.TemplateBox, api.SecretsPut, api.ConfigPut, api.ServerInit, api.ServerGC,
	api.WorkerRm, api.MachineToken}

// gate counts the changing calls under way; once closed it refuses new ones.
type gate struct {
	mu     sync.Mutex
	n      int
	closed bool
}

func (g *gate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.n++
	return true
}

func (g *gate) leave() {
	g.mu.Lock()
	g.n--
	g.mu.Unlock()
}

func (g *gate) setClosed(c bool) {
	g.mu.Lock()
	g.closed = c
	g.mu.Unlock()
}

func (g *gate) state() (closed bool, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed, g.n
}

// gated wraps the changing calls so a handover can stop them.
func (s *Server) gated(mux rpc.Mux) {
	for _, m := range changing {
		h := mux[m]
		mux[m] = func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			if !s.gate.enter() {
				return nil, errors.New("this machine is handing the lead to another: try again in a minute")
			}
			defer s.gate.leave()
			return h(ctx, params, out)
		}
	}
}

// pause stops changes: new ones are refused, and those under way get timeout to
// finish, or the pause is called off and changes go on.
func (s *Server) pause(timeout time.Duration) error {
	s.gate.setClosed(true)
	deadline := time.Now().Add(timeout)
	for {
		if _, n := s.gate.state(); n == 0 {
			return nil
		} else if time.Now().After(deadline) {
			s.gate.setClosed(false)
			return fmt.Errorf("%d changes still under way after %s (machines.handover_timeout)", n, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// machineLead hands the lead to a connected worker: changes and login refreshes
// stop, a consistent copy of the database goes to it with the master key, and
// this machine becomes its worker. Boxes keep running throughout.
func (s *Server) machineLead(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.MachineLeadParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	stdout, _ := out.Writers()
	s.mu.Lock()
	local := s.local
	s.mu.Unlock()
	if p.ID == local {
		return nil, errors.New("this machine already leads")
	}
	w, err := s.worker(p.ID)
	if err != nil {
		return nil, fmt.Errorf("%s isn't connected, so it can't take the lead: for a lead that's dead, hutch machine lead <ssh-host> --from-backup", p.ID)
	}
	if w.info.Version != version.String() {
		return nil, fmt.Errorf("%s runs hutchd %s and this lead %s: upgrade it first", w.info.Name, w.info.Version, version.String())
	}
	if p.Address == "" {
		return nil, fmt.Errorf("no address for %s that other machines can dial", w.info.Name)
	}
	v := s.vault.Load()
	l, err := s.store.link()
	if v == nil || l == nil || err != nil {
		return nil, fmt.Errorf("a handover needs a bucket and the worker link: %v", err)
	}
	cfg, err := s.settings()
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(p.Address, strconv.FormatInt(cfg.Int("machine.link_port"), 10))

	fmt.Fprintf(stdout, "Stopping changes (calls under way get %s)...\n", cfg.Duration("machines.handover_timeout"))
	if err := s.pause(cfg.Duration("machines.handover_timeout")); err != nil {
		return nil, fmt.Errorf("handover called off, nothing changed: %w", err)
	}
	done := false
	defer func() {
		if !done {
			s.gate.setClosed(false)
			s.keyProxy.Undrain()
		}
	}()
	fmt.Fprintf(stdout, "Waiting for login refreshes under way to be stored...\n")
	dctx, cancel := context.WithTimeout(ctx, cfg.Duration("keys.refresh_timeout"))
	err = s.keyProxy.Drain(dctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("handover called off, nothing changed: %w", err)
	}
	if p.Self != "" {
		if _, err := s.store.exec(`UPDATE workers SET ssh = ? WHERE id = ? AND ssh = ''`, p.Self, local); err != nil {
			return nil, fmt.Errorf("handover called off, nothing changed: %w", err)
		}
	}
	// The new lead's term, above any in the bucket.
	h, ok, err := link.HighestRecord(ctx, v.bucket, l.ca.Cert)
	if err != nil {
		return nil, fmt.Errorf("handover called off, nothing changed: %w", err)
	}
	term := l.term + 1
	if ok && h.Term >= term {
		term = h.Term + 1
	}
	db, err := s.handoverCopy(ctx, local, p.Address, term)
	if err != nil {
		return nil, fmt.Errorf("handover called off, nothing changed: %w", err)
	}
	fmt.Fprintf(stdout, "Handing %s the database (%.1f MB) and the master key...\n", w.info.Name, float64(len(db))/(1<<20))
	if err := rpc.Call(ctx, w.sess, api.WorkerTakeLead, api.TakeLeadParams{DB: db, Master: s.master[:], Bucket: v.cfg}, nil); err != nil {
		return nil, fmt.Errorf("handover called off, nothing changed: %s: %w", w.info.Name, err)
	}
	// From here the other machine leads; this one doesn't go back. Its record
	// goes in now, so no worker follows the old one back here.
	done = true
	rec := link.Record{Term: term, Address: addr, WorkerID: w.id}
	s.superseded.Store(&rec)
	if err := link.PutRecord(ctx, v.bucket, l.ca, rec, false); err != nil {
		log.Printf("lead record for %s: %v (it writes its own)", w.info.Name, err)
	}
	s.store.event("", "lead", fmt.Sprintf("handed the lead to %s", w.info.Name))
	s.mu.Lock()
	var others []*workerConn
	for id, o := range s.workers {
		if id != local && id != w.id {
			others = append(others, o)
		}
	}
	s.mu.Unlock()
	for _, o := range others {
		if err := rpcCall(ctx, o, api.WorkerLead, api.LeadParams{Address: addr, Term: term}); err != nil {
			log.Printf("worker %s: point at the new lead: %v (it follows the lead record)", o.id, err)
		}
	}
	if err := s.becomeWorker(l, local, addr, v.cfg); err != nil {
		return nil, fmt.Errorf("%s leads now, but this machine couldn't become its worker: %w (hutch machine add it there)", w.info.Name, err)
	}
	fmt.Fprintf(stdout, "%s leads; this machine restarts as its worker.\n", w.info.Name)
	if s.cfg.Restart != nil {
		time.AfterFunc(time.Second, s.cfg.Restart)
	}
	return nil, nil
}

// handoverCopy is a consistent copy of the database for the new lead at
// address and term: this machine is one of its linked workers there.
func (s *Server) handoverCopy(ctx context.Context, local, address string, term int64) ([]byte, error) {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	tmp := filepath.Join(s.cfg.Dir, "handover.db")
	os.Remove(tmp)
	defer os.Remove(tmp)
	if _, err := s.store.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return nil, err
	}
	if err := prepareHandover(tmp, local, address, term); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp)
}

func prepareHandover(path, local, address string, term int64) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE workers SET linked = 1 WHERE id = ?`, local); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE link SET term = ?, address = ? WHERE id = 1`, term, address)
	return err
}

// becomeWorker enrolls this machine with the lead at addr, with a certificate
// for its own worker ID, and keeps its database and master key aside.
func (s *Server) becomeWorker(l *linkState, local, addr string, b bucket.Config) error {
	ws, err := s.store.workers()
	if err != nil {
		return err
	}
	name := local
	for _, w := range ws {
		if w.ID == local {
			name = w.Name
		}
	}
	keyPEM, csr, err := pki.NewWorkerKey(name)
	if err != nil {
		return err
	}
	cert, err := l.ca.SignWorker(csr, local)
	if err != nil {
		return err
	}
	root := filepath.Dir(s.cfg.Dir)
	dir := link.Dir(filepath.Join(root, "link.new"))
	os.RemoveAll(string(dir))
	if err := os.MkdirAll(string(dir), 0o700); err != nil {
		return err
	}
	if err := dir.SaveBucket(b); err != nil {
		return err
	}
	if err := link.Write(dir, keyPEM, cert, l.ca.PEM, name, addr); err != nil {
		return err
	}
	if err := setAside(s.cfg.Dir, fmt.Sprintf("stale-lead-%d", l.term)); err != nil {
		return err
	}
	return os.Rename(string(dir), filepath.Join(root, "link"))
}

// setAside moves a machine's lead state (database, master key, bucket) into a
// directory of dir, so it never leads with it again; nothing is deleted. The
// link CA's certificate goes beside it, as ca.pem: hutch machine add knows the
// machine by it as one of this setup's, so its boxes are taken back.
func setAside(dir, name string) error {
	aside := filepath.Join(dir, name)
	if _, err := os.Stat(aside); err == nil {
		aside += "-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	if ca, err := linkCACert(filepath.Join(dir, "hutch.db")); err != nil {
		log.Printf("lead state: reading the link CA: %v", err)
	} else if len(ca) > 0 {
		if err := os.WriteFile(filepath.Join(aside, "ca.pem"), ca, 0o600); err != nil {
			return err
		}
	}
	for _, f := range []string{"hutch.db", "hutch.db-wal", "hutch.db-shm", "master.key", "bucket.json"} {
		if err := os.Rename(filepath.Join(dir, f), filepath.Join(aside, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	log.Printf("lead state kept aside in %s (hutch machine forget-stale removes it)", aside)
	return nil
}

// SetAside is setAside for hutchd: a fenced lead's state goes aside on start.
func SetAside(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "hutch.db")); err != nil {
		return nil
	}
	return setAside(dir, "stale-lead-"+strconv.FormatInt(time.Now().Unix(), 10))
}

// linkCACert is the link CA's certificate in a lead's database, or nothing for
// one that never had workers.
func linkCACert(path string) ([]byte, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var ca []byte
	err = db.QueryRow(`SELECT ca_cert FROM link WHERE id = 1`).Scan(&ca)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return ca, err
}
