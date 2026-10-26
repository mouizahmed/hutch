//go:build linux

package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/worker"
)

// vault is what hutchd needs to reach the bucket: the store and the keys of the
// projects it hosts, from the server, in memory only.
type vault struct {
	store *durable.Store
	keys  map[string]durable.Key
}

// uploader stores boxes' new generations in the bucket, one box at a time, so
// checkpoints don't compete with each other for the worker's bandwidth.
type uploader struct {
	w     *worker.Worker
	vault atomic.Pointer[vault]
	kicks chan struct{}
}

func newUploader(w *worker.Worker) *uploader {
	return &uploader{w: w, kicks: make(chan struct{}, 1)}
}

func (u *uploader) kick() {
	select {
	case u.kicks <- struct{}{}:
	default:
	}
}

// configure applies the bucket settings from the server.
func (u *uploader) configure(p *api.WorkerSettingsParams) {
	if p.Bucket == nil {
		u.vault.Store(nil)
		return
	}
	b, err := bucket.New(*p.Bucket)
	if err != nil {
		log.Printf("bucket: %v", err)
		return
	}
	v := &vault{store: &durable.Store{Bucket: b, Refresh: p.Refresh,
		Cache: &durable.Cache{Dir: filepath.Join(u.w.Root, "cache", "chunks"), Limit: cacheLimit(u.w.Root, p.ChunkCache)}},
		keys: map[string]durable.Key{}}
	for project, raw := range p.Keys {
		if k, err := durable.ParseKey(raw); err == nil {
			v.keys[project] = k
		}
	}
	u.vault.Store(v)
	u.kick()
}

func cacheLimit(root string, percent float64) int64 {
	return int64(float64(diskUsage(root).Total) * percent / 100)
}

// key returns the store and a project's key, or false without them.
func (u *uploader) key(project string) (*durable.Store, durable.Key, bool) {
	v := u.vault.Load()
	if v == nil {
		return nil, durable.Key{}, false
	}
	k, ok := v.keys[project]
	return v.store, k, ok
}

func (u *uploader) run(ctx context.Context) {
	failed := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.kicks:
		case <-time.After(time.Minute):
		}
		boxes, err := u.w.List()
		if err != nil {
			continue
		}
		for _, b := range boxes {
			if !b.Pending() || time.Since(failed[b.ID]) < time.Minute {
				continue
			}
			s, k, ok := u.key(b.Store.Project)
			if !ok {
				continue
			}
			start := time.Now()
			st, err := u.w.Upload(ctx, b.ID, s, k)
			if err != nil {
				log.Printf("upload %s: %v", b.ID, err)
				failed[b.ID] = time.Now()
				continue
			}
			delete(failed, b.ID)
			var parts []string
			for _, name := range []string{"rootfs.ext4", "mem", "vmstate"} {
				if f, ok := st[name]; ok {
					parts = append(parts, fmt.Sprintf("%s read %d of %d chunks, wrote %d (%d MB)", name, f.Read, f.Chunks, f.Uploaded, f.Bytes>>20))
				}
			}
			log.Printf("uploaded %s gen %d in %s: %s", b.ID, b.Generation, time.Since(start).Round(time.Millisecond), strings.Join(parts, "; "))
		}
	}
}
