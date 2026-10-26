package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"

	"github.com/mouizahmed/hutch/internal/bucket"
)

// ChunkSize is part of the bucket format: files are cut into chunks of this size.
const ChunkSize = 4 << 20

// Format is the bucket format version this build writes; it reads every older one.
const Format = 1

const parallel = 8

var (
	encoder, _ = zstd.NewWriter(nil)
	decoder, _ = zstd.NewReader(nil)
)

// Store reads and writes chunks and manifests in a bucket.
type Store struct {
	Bucket bucket.Bucket
	// Cache, when set, keeps downloaded chunks on local disk.
	Cache *Cache
	// Refresh: a chunk that already exists but is older than this is written again,
	// so garbage collection (which spares chunks younger than its grace period, at
	// least twice this) can't delete it while a manifest that uses it is in flight.
	Refresh time.Duration
}

func ChunkKey(id string) string { return "chunks/" + id }

// Manifest lists every chunk of a set of files: complete, never a diff.
type Manifest struct {
	Format  int              `json:"format"`
	Created time.Time        `json:"created"`
	Box     *BoxInfo         `json:"box,omitempty"`
	Files   map[string]*File `json:"files"`
}

// BoxInfo describes the VM a box manifest restores.
type BoxInfo struct {
	Image     string `json:"image"`
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	FCVersion string `json:"fc_version"`
	Class     string `json:"snapshot_class"`
	// Archived boxes keep only their disk; waking them is a cold boot.
	Archived bool `json:"archived,omitempty"`
	// The box's policies, so it restores anywhere as it was.
	IdleTimeout  time.Duration `json:"idle_timeout,omitempty"`
	CPUThreshold float64       `json:"cpu_threshold,omitempty"`
	Allow        []string      `json:"allow,omitempty"`
	Watch        []string      `json:"watch,omitempty"`
}

// File is one file's size and chunk ids; "" is a chunk of zeros, never stored.
type File struct {
	Size   int64    `json:"size"`
	Chunks []string `json:"chunks"`
}

// Chunks returns every chunk id the manifest uses.
func (m *Manifest) Chunks() []string {
	var ids []string
	for _, f := range m.Files {
		for _, id := range f.Chunks {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// Init writes the bucket's format file, or checks that this build can read it.
func (s *Store) Init(ctx context.Context) error {
	data, _ := json.Marshal(map[string]int{"format": Format})
	err := s.Bucket.Put(ctx, "format", data, true)
	if !errors.Is(err, bucket.ErrExists) {
		return err
	}
	return s.CheckFormat(ctx)
}

func (s *Store) CheckFormat(ctx context.Context) error {
	data, err := s.Bucket.Get(ctx, "format")
	if err != nil {
		return err
	}
	var f struct{ Format int }
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("bucket format file: %w", err)
	}
	if f.Format > Format {
		return fmt.Errorf("the bucket has format %d; this Hutch reads up to %d, upgrade it", f.Format, Format)
	}
	return nil
}

func (s *Store) PutManifest(ctx context.Context, k Key, name string, m *Manifest) error {
	m.Format = Format
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.Bucket.Put(ctx, name, k.Seal(name, encoder.EncodeAll(data, nil)), true)
}

func (s *Store) GetManifest(ctx context.Context, k Key, name string) (*Manifest, error) {
	sealed, err := s.Bucket.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return OpenManifest(k, name, sealed)
}

func OpenManifest(k Key, name string, sealed []byte) (*Manifest, error) {
	z, err := k.Open(name, sealed)
	if err != nil {
		return nil, err
	}
	data, err := decoder.DecodeAll(z, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if m.Format > Format {
		return nil, fmt.Errorf("%s has format %d; this Hutch reads up to %d", name, m.Format, Format)
	}
	return &m, nil
}

// PutObject and GetObject store one small sealed object, like a project's name record.
func (s *Store) PutObject(ctx context.Context, k Key, name string, data []byte) error {
	return s.Bucket.Put(ctx, name, k.Seal(name, data), false)
}

func (s *Store) GetObject(ctx context.Context, k Key, name string) ([]byte, error) {
	sealed, err := s.Bucket.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return k.Open(name, sealed)
}

// Change is what a caller knows about one chunk of a file before reading it.
type Change int

const (
	Changed Change = iota // read it and store it if new
	Same                  // the reference file's chunk is still right
	Hole                  // all zeros, without reading
)

// Stats counts what a PutFile did.
type Stats struct {
	Chunks   int   // chunks in the file
	Read     int   // chunks read and hashed
	Uploaded int   // chunks written to the bucket
	Bytes    int64 // bytes written to the bucket
}

// PutFile stores a file's chunks under k and returns its manifest entry. change
// (nil: everything changed) says which chunks to reuse from ref.
func (s *Store) PutFile(ctx context.Context, k Key, f io.ReaderAt, size int64, ref *File, change func(i int) Change) (*File, Stats, error) {
	n := int((size + ChunkSize - 1) / ChunkSize)
	out := &File{Size: size, Chunks: make([]string, n)}
	st := Stats{Chunks: n}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for i := 0; i < n; i++ {
		c := Changed
		if change != nil {
			c = change(i)
		}
		if c == Same && (ref == nil || i >= len(ref.Chunks) || ref.Size != size) {
			c = Changed
		}
		switch c {
		case Hole:
			continue
		case Same:
			out.Chunks[i] = ref.Chunks[i]
			continue
		}
		g.Go(func() error {
			buf := make([]byte, min(ChunkSize, size-int64(i)*ChunkSize))
			if _, err := f.ReadAt(buf, int64(i)*ChunkSize); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if allZero(buf) {
				mu.Lock()
				st.Read++
				mu.Unlock()
				return nil
			}
			id, wrote, err := s.putChunk(gctx, k, buf)
			if err != nil {
				return err
			}
			mu.Lock()
			out.Chunks[i] = id
			st.Read++
			if wrote > 0 {
				st.Uploaded++
				st.Bytes += int64(wrote)
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, st, err
	}
	return out, st, nil
}

// putChunk stores a chunk unless it is already there (and fresh), returning its id
// and the bytes written.
func (s *Store) putChunk(ctx context.Context, k Key, plain []byte) (string, int, error) {
	id := k.ChunkID(plain)
	body := k.Seal(id, encoder.EncodeAll(plain, make([]byte, 0, len(plain)/2)))
	err := s.Bucket.Put(ctx, ChunkKey(id), body, true)
	if errors.Is(err, bucket.ErrExists) {
		obj, herr := s.Bucket.Head(ctx, ChunkKey(id))
		if herr != nil {
			return "", 0, herr
		}
		if s.Refresh == 0 || time.Since(obj.Modified) < s.Refresh {
			return id, 0, nil
		}
		err = s.Bucket.Put(ctx, ChunkKey(id), body, false)
	}
	if err != nil {
		return "", 0, err
	}
	return id, len(body), nil
}

// GetFile writes a file from its manifest entry into dst, leaving zero chunks as holes.
func (s *Store) GetFile(ctx context.Context, k Key, f *File, dst *os.File) error {
	if err := dst.Truncate(f.Size); err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for i, id := range f.Chunks {
		if id == "" {
			continue
		}
		g.Go(func() error {
			plain, err := s.GetChunk(gctx, k, id)
			if err != nil {
				return err
			}
			_, err = dst.WriteAt(plain, int64(i)*ChunkSize)
			return err
		})
	}
	return g.Wait()
}

// GetChunk reads a chunk, from the cache when it has it, and checks it against its id.
func (s *Store) GetChunk(ctx context.Context, k Key, id string) ([]byte, error) {
	var body []byte
	if s.Cache != nil {
		body = s.Cache.get(id)
	}
	cached := body != nil
	if !cached {
		var err error
		if body, err = s.Bucket.Get(ctx, ChunkKey(id)); err != nil {
			return nil, err
		}
	}
	z, err := k.Open(id, body)
	if err != nil {
		return nil, err
	}
	plain, err := decoder.DecodeAll(z, make([]byte, 0, ChunkSize))
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", id, err)
	}
	if k.ChunkID(plain) != id {
		return nil, fmt.Errorf("chunk %s: contents don't match its id", id)
	}
	if s.Cache != nil && !cached {
		s.Cache.put(id, body)
	}
	return plain, nil
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if b[0]|b[1]|b[2]|b[3]|b[4]|b[5]|b[6]|b[7] != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Cache keeps sealed chunks in a directory, evicting the least recently used
// beyond Limit bytes.
type Cache struct {
	Dir   string
	Limit int64
	mu    sync.Mutex
}

func (c *Cache) path(id string) string { return filepath.Join(c.Dir, id) }

func (c *Cache) get(id string) []byte {
	data, err := os.ReadFile(c.path(id))
	if err != nil {
		return nil
	}
	now := time.Now()
	os.Chtimes(c.path(id), now, now)
	return data
}

func (c *Cache) put(id string, body []byte) {
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return
	}
	tmp := c.path(id) + ".tmp"
	if os.WriteFile(tmp, body, 0o600) == nil {
		os.Rename(tmp, c.path(id))
	}
}

// Evict removes the least recently used chunks until the cache fits its limit.
func (c *Cache) Evict() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var all []entry
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		all = append(all, entry{c.path(e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].used.Before(all[j].used) })
	for _, e := range all {
		if total <= c.Limit {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
	return nil
}
