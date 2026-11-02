package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/rpc"
)

var errNoBucket = errors.New("the machine has no bucket: hutch machine bucket")

// loadMaster reads the master key, creating it on first start. It never leaves the
// server except wrapped by the recovery passphrase.
func loadMaster(dir string) (durable.Key, error) {
	path := filepath.Join(dir, "master.key")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		k := durable.NewKey()
		return k, os.WriteFile(path, k[:], 0o600)
	}
	if err != nil {
		return durable.Key{}, err
	}
	return durable.ParseKey(data)
}

func loadBucket(dir string) (bucket.Bucket, *bucket.Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, "bucket.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var cfg bucket.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("bucket.json: %w", err)
	}
	b, err := bucket.New(cfg)
	return b, &cfg, err
}

func writeBucket(dir string, cfg bucket.Config) error {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := filepath.Join(dir, "bucket.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "bucket.json"))
}

type vault struct {
	bucket bucket.Bucket
	cfg    bucket.Config
}

// bucketStore returns the store for the server's bucket, refreshing reused chunks
// older than half the GC grace period.
func (s *Server) bucketStore() (*durable.Store, error) {
	v := s.vault.Load()
	if v == nil {
		return nil, errNoBucket
	}
	cfg, err := s.settings()
	if err != nil {
		return nil, err
	}
	return &durable.Store{Bucket: v.bucket, Refresh: cfg.Duration("gc.grace") / 2}, nil
}

func (s *Server) projectPrefix(project string) string { return s.master.ProjectPrefix(project) }

// boxStore is where a project box's snapshots go, by its bucket ID and ownership
// number; boxes outside projects aren't stored.
func (s *Server) boxStore(project, uid, owner string) *api.Store {
	if project == "" {
		return nil
	}
	return &api.Store{Project: project, Prefix: s.projectPrefix(project) + "/boxes/" + uid, Owner: owner}
}

func (s *Server) versionStore(project, class string, version int) *api.Store {
	return &api.Store{Project: project, Prefix: fmt.Sprintf("%s/templates/%s/v%d", s.projectPrefix(project), class, version)}
}

// projectRecord writes a project's ID, sealed, under its prefix, so garbage
// collection can find the key to its manifests from the bucket alone.
func (s *Server) projectRecord(ctx context.Context, project string) {
	if _, done := s.recorded.Load(project); done {
		return
	}
	st, err := s.bucketStore()
	if err != nil {
		return
	}
	if err := st.PutObject(ctx, s.master.ServerKey(), s.projectPrefix(project)+"/id", []byte(project)); err != nil {
		log.Printf("project record %s: %v", project, err)
		return
	}
	s.recorded.Store(project, true)
}

func (s *Server) serverInit(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ServerInitParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if s.vault.Load() != nil {
		return nil, errors.New("the machine already has a bucket")
	}
	if len(p.Passphrase) < api.MinPassphrase {
		return nil, fmt.Errorf("the recovery passphrase needs at least %d characters", api.MinPassphrase)
	}
	b, err := bucket.New(p.Bucket)
	if err != nil {
		return nil, err
	}
	if err := checkBucket(ctx, b); err != nil {
		return nil, err
	}
	if _, err := b.Head(ctx, "server/master"); err == nil {
		return nil, errors.New("this bucket already holds a Hutch machine's keys: restore from it with hutchd restore, or use an empty bucket")
	} else if !errors.Is(err, bucket.ErrNotFound) {
		return nil, err
	}
	st := &durable.Store{Bucket: b}
	if err := st.Init(ctx); err != nil {
		return nil, err
	}
	if err := b.Put(ctx, "server/master", durable.Wrap(s.master, p.Passphrase).Marshal(), true); err != nil {
		return nil, err
	}
	if err := writeBucket(s.cfg.Dir, p.Bucket); err != nil {
		return nil, err
	}
	s.vault.Store(&vault{bucket: b, cfg: p.Bucket})
	if err := s.backup(ctx); err != nil {
		return nil, fmt.Errorf("first backup: %w", err)
	}
	for _, w := range s.connectedWorkers() {
		s.sendWorkerSettings(ctx, w)
	}
	log.Printf("bucket %s initialized", p.Bucket.Name)
	return nil, nil
}

// checkBucket makes sure the bucket is reachable and supports conditional writes,
// which manifests rely on to never overwrite each other.
func checkBucket(ctx context.Context, b bucket.Bucket) error {
	probe := "probe/" + randomHex(8)
	if err := b.Put(ctx, probe, []byte("probe"), true); err != nil {
		return fmt.Errorf("bucket: %w", err)
	}
	defer b.Delete(ctx, probe)
	if err := b.Put(ctx, probe, []byte("probe"), true); !errors.Is(err, bucket.ErrExists) {
		return fmt.Errorf("the bucket doesn't support conditional writes (If-None-Match); Hutch needs them (%v)", err)
	}
	return nil
}

func (s *Server) connectedWorkers() []*workerConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workerConn, 0, len(s.workers))
	for _, w := range s.workers {
		out = append(out, w)
	}
	return out
}

// backupLoop backs the server up to the bucket every backup.interval when the
// database changed.
func (s *Server) backupLoop(ctx context.Context) {
	last := int64(-1)
	for {
		interval := time.Minute
		if cfg, err := s.settings(); err == nil {
			interval = cfg.Duration("backup.interval")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-s.backupNow:
		}
		if s.vault.Load() == nil {
			continue
		}
		n, err := s.store.changes()
		if err != nil || n == last {
			continue
		}
		if err := s.backup(ctx); err != nil {
			log.Printf("backup: %v", err)
			s.store.event("", "backup", "failed: "+err.Error())
			continue
		}
		last = n
	}
}

// backup writes a consistent copy of the database and the server's CA to the bucket.
func (s *Server) backup(ctx context.Context) error {
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if err := s.checkLead(ctx); err != nil {
		return err
	}
	st, err := s.bucketStore()
	if err != nil {
		return err
	}
	changes, _ := s.store.changes()
	tmp := filepath.Join(s.cfg.Dir, "backup.db")
	os.Remove(tmp)
	defer os.Remove(tmp)
	if _, err := s.store.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return err
	}
	now := time.Now().UTC()
	m := &durable.Manifest{Created: now, Files: map[string]*durable.File{}}
	paths := map[string]string{"hutch.db": tmp}
	k := s.master.ServerKey()
	for name, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err == nil {
			m.Files[name], _, err = st.PutFile(ctx, k, f, fi.Size(), nil, nil)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := st.PutManifest(ctx, k, backupName(s.store.term(), now), m); err != nil {
		return err
	}
	s.lastBackup, s.backedUp = now, changes
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	return pruneBackups(ctx, st.Bucket, now, cfg.Duration("retention.backups"))
}

// pruneBackups keeps every backup from the last hour, the newest of each hour for a
// day, and the newest of each day up to keep; the newest backup always stays.
func pruneBackups(ctx context.Context, b bucket.Bucket, now time.Time, keep time.Duration) error {
	objs, err := b.List(ctx, "server/backups/")
	if err != nil {
		return err
	}
	sort.Slice(objs, func(i, j int) bool { return newerBackup(objs[i].Key, objs[j].Key) })
	seen := map[string]bool{}
	for i, o := range objs {
		_, t, ok := parseBackup(o.Key)
		if !ok {
			continue
		}
		age := now.Sub(t)
		var slot string
		switch {
		case i == 0 || age < time.Hour:
			continue
		case age < 24*time.Hour:
			slot = t.Format("2006-01-02T15")
		case age < keep:
			slot = t.Format("2006-01-02")
		}
		if slot != "" && !seen[slot] {
			seen[slot] = true
			continue
		}
		if err := b.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// Restore recovers a server's data directory from its bucket: the master key with
// the recovery passphrase, then the newest backup. It returns when that backup was taken.
func Restore(ctx context.Context, dir string, cfg bucket.Config, passphrase string) (time.Time, error) {
	if _, err := os.Stat(filepath.Join(dir, "hutch.db")); err == nil {
		return time.Time{}, fmt.Errorf("%s already holds a machine's state", dir)
	}
	b, err := bucket.New(cfg)
	if err != nil {
		return time.Time{}, err
	}
	st := &durable.Store{Bucket: b}
	if err := st.CheckFormat(ctx); err != nil {
		return time.Time{}, err
	}
	data, err := b.Get(ctx, "server/master")
	if err != nil {
		return time.Time{}, fmt.Errorf("no Hutch machine in this bucket: %w", err)
	}
	var r durable.Recovery
	if err := json.Unmarshal(data, &r); err != nil {
		return time.Time{}, err
	}
	master, err := r.Unwrap(passphrase)
	if err != nil {
		return time.Time{}, err
	}
	objs, err := b.List(ctx, "server/backups/")
	if err != nil {
		return time.Time{}, err
	}
	if len(objs) == 0 {
		return time.Time{}, errors.New("the bucket has no backups")
	}
	// The newest backup of the highest term: a stale lead's later ones lose.
	sort.Slice(objs, func(i, j int) bool { return newerBackup(objs[i].Key, objs[j].Key) })
	m, err := st.GetManifest(ctx, master.ServerKey(), objs[0].Key)
	if err != nil {
		return time.Time{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return time.Time{}, err
	}
	for name, file := range m.Files {
		f, err := os.OpenFile(filepath.Join(dir, filepath.Base(name)), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return time.Time{}, err
		}
		err = st.GetFile(ctx, master.ServerKey(), file, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("restore %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "master.key"), master[:], 0o600); err != nil {
		return time.Time{}, err
	}
	// This is a new machine: the old one's boxes restore here from the bucket.
	// Linked workers keep theirs. It leads at a term of its own, above the
	// highest in the bucket, which it claims when it starts.
	db, err := openStore(filepath.Join(dir, "hutch.db"), master.Derive("user-layer"))
	if err != nil {
		return time.Time{}, err
	}
	_, err = db.db.Exec(`UPDATE boxes SET state = 'asleep' WHERE state = 'running' AND worker_id IN (SELECT id FROM workers WHERE linked = 0);
		UPDATE workers SET gone = 1 WHERE linked = 0;
		UPDATE link SET term = 0`)
	db.db.Close()
	if err != nil {
		return time.Time{}, err
	}
	return m.Created, writeBucket(dir, cfg)
}

func (s *Server) bucketGet(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	var info api.BucketInfo
	if v := s.vault.Load(); v != nil {
		info.Name = v.cfg.Name
	}
	return info, nil
}

func (s *Server) gcLoop(ctx context.Context) {
	for {
		interval := 24 * time.Hour
		if cfg, err := s.settings(); err == nil {
			interval = cfg.Duration("gc.interval")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if s.vault.Load() == nil {
			continue
		}
		if r, err := s.gc(ctx); err != nil {
			log.Printf("gc: %v", err)
			s.store.event("", "gc", "failed: "+err.Error())
		} else {
			log.Printf("gc: %d manifests (%d pruned), deleted %d of %d chunks (%d MB)", r.Manifests, r.Pruned, r.Deleted, r.Chunks, r.Bytes>>20)
		}
	}
}

func (s *Server) serverGC(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	return s.gc(ctx)
}

// gc applies box snapshot retention, then deletes the chunks nothing uses.
func (s *Server) gc(ctx context.Context) (api.GCResult, error) {
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	var r api.GCResult
	if err := s.checkLead(ctx); err != nil {
		return r, err
	}
	st, err := s.bucketStore()
	if err != nil {
		return r, err
	}
	cfg, err := s.settings()
	if err != nil {
		return r, err
	}
	if r.Pruned, err = pruneBoxSnapshots(ctx, st.Bucket, int(cfg.Int("retention.box_checkpoints"))); err != nil {
		return r, err
	}
	var mu sync.Mutex
	keys := map[string]durable.Key{}
	keyFor := func(ctx context.Context, name string) (durable.Key, bool, error) {
		parts := strings.Split(name, "/")
		switch {
		case len(parts) == 3 && parts[0] == "server" && parts[1] == "backups":
			return s.master.ServerKey(), true, nil
		case parts[0] != "projects":
			return durable.Key{}, false, nil
		case len(parts) < 4 || (parts[2] != "boxes" && parts[2] != "templates"):
			if len(parts) == 3 && parts[2] == "id" {
				return durable.Key{}, false, nil
			}
			return durable.Key{}, false, fmt.Errorf("unexpected object")
		}
		mu.Lock()
		defer mu.Unlock()
		if k, ok := keys[parts[1]]; ok {
			return k, true, nil
		}
		id, err := st.GetObject(ctx, s.master.ServerKey(), "projects/"+parts[1]+"/id")
		if err != nil {
			return durable.Key{}, false, fmt.Errorf("project record: %w", err)
		}
		k := s.master.ProjectKey(string(id))
		keys[parts[1]] = k
		return k, true, nil
	}
	gs, err := st.GC(ctx, keyFor, cfg.Duration("gc.grace"))
	r.Manifests, r.Chunks, r.Deleted, r.Bytes = gs.Manifests, gs.Chunks, gs.Deleted, gs.Bytes
	return r, err
}

// pruneBoxSnapshots keeps the newest keep snapshots of every box. A box's newest is
// the one it restores from, and earlier ones are fallbacks.
func pruneBoxSnapshots(ctx context.Context, b bucket.Bucket, keep int) (int, error) {
	objs, err := b.List(ctx, "projects/")
	if err != nil {
		return 0, err
	}
	type snapshot struct {
		name, owner string
		gen         int
	}
	byBox := map[string][]snapshot{}
	for _, o := range objs {
		if parts := strings.Split(o.Key, "/"); len(parts) == 5 && parts[2] == "boxes" {
			owner, gen, ok := durable.ParseManifestName(parts[4])
			if !ok {
				continue
			}
			dir := strings.Join(parts[:4], "/")
			byBox[dir] = append(byBox[dir], snapshot{o.Key, owner, gen})
		}
	}
	pruned := 0
	for _, snaps := range byBox {
		sort.Slice(snaps, func(i, j int) bool {
			return durable.Newer(snaps[i].owner, snaps[i].gen, snaps[j].owner, snaps[j].gen)
		})
		for _, sn := range snaps[min(keep, len(snaps)):] {
			n := sn.name
			if err := b.Delete(ctx, n); err != nil {
				return pruned, err
			}
			pruned++
		}
	}
	return pruned, nil
}

// deletePrefix removes every object under prefix: a removed box's or project's manifests.
// Their chunks go at the next collection.
func (s *Server) deletePrefix(ctx context.Context, prefix string) {
	v := s.vault.Load()
	if v == nil {
		return
	}
	objs, err := v.bucket.List(ctx, prefix+"/")
	if err != nil {
		log.Printf("delete %s: %v", prefix, err)
		return
	}
	for _, o := range objs {
		if err := v.bucket.Delete(ctx, o.Key); err != nil {
			log.Printf("delete %s: %v", o.Key, err)
		}
	}
}

// latestSnapshot returns the newest snapshot of a store in the bucket: the newest
// generation of the highest ownership number.
func (s *Server) latestSnapshot(ctx context.Context, store *api.Store) (owner string, gen int, err error) {
	v := s.vault.Load()
	if v == nil {
		return "", 0, errNoBucket
	}
	objs, err := v.bucket.List(ctx, store.Prefix+"/")
	if err != nil {
		return "", 0, err
	}
	for _, o := range objs {
		if ow, n, ok := durable.ParseManifestName(strings.TrimPrefix(o.Key, store.Prefix+"/")); ok && durable.Newer(ow, n, owner, gen) {
			owner, gen = ow, n
		}
	}
	if gen == 0 {
		return "", 0, errors.New("the bucket has no snapshot of it")
	}
	return owner, gen, nil
}

// storedGeneration returns the newest generation in the bucket written by the
// store's own owner.
func (s *Server) storedGeneration(ctx context.Context, store *api.Store) (int, error) {
	v := s.vault.Load()
	if v == nil {
		return 0, errNoBucket
	}
	objs, err := v.bucket.List(ctx, store.Prefix+"/")
	if err != nil {
		return 0, err
	}
	gen := 0
	for _, o := range objs {
		if ow, n, ok := durable.ParseManifestName(strings.TrimPrefix(o.Key, store.Prefix+"/")); ok && ow == store.Owner {
			gen = max(gen, n)
		}
	}
	return gen, nil
}

// claim gives a box a new ownership number before it is placed on another
// machine, with its snapshot from (owner, gen) stored again under that number,
// so the new copy has it and the old copy's later writes never mix with its own.
// It returns the store the new copy writes to.
func (s *Server) claim(ctx context.Context, store *api.Store, owner string, gen int) (*api.Store, error) {
	st, err := s.bucketStore()
	if err != nil {
		return nil, err
	}
	next, err := s.store.nextOwner(s.store.term())
	if err != nil {
		return nil, err
	}
	k := s.master.ProjectKey(store.Project)
	m, err := st.GetManifest(ctx, k, durable.ManifestName(store.Prefix, owner, gen))
	if err != nil {
		return nil, err
	}
	if err := st.PutManifest(ctx, k, durable.ManifestName(store.Prefix, next, gen), m); err != nil && !errors.Is(err, bucket.ErrExists) {
		return nil, err
	}
	claimed := *store
	claimed.Owner = next
	return &claimed, nil
}
