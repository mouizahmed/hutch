package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/mouizahmed/hutch/internal/bucket"
)

type GCStats struct {
	Manifests int
	Chunks    int
	Deleted   int
	Bytes     int64
}

// ManifestKey says whether a bucket object is a manifest and returns the key that
// opens it.
type ManifestKey func(ctx context.Context, name string) (Key, bool, error)

// GC deletes every chunk that no manifest uses and that is older than grace. Chunks
// are listed before manifests are read, so a chunk written for a manifest that isn't
// there yet is young and spared. A manifest that can't be opened stops the sweep:
// its chunks can't be told apart. Deletes match the listed ETag so a concurrent
// upload that refreshes an old chunk protects it from this sweep.
func (s *Store) GC(ctx context.Context, keyFor ManifestKey, grace time.Duration) (GCStats, error) {
	var st GCStats
	if err := bucket.CheckConditionalDelete(ctx, s.Bucket); err != nil {
		return st, err
	}
	cutoff := time.Now().Add(-grace)
	objs, err := s.Bucket.List(ctx, "")
	if err != nil {
		return st, err
	}
	used := map[string]bool{}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for _, o := range objs {
		if strings.HasPrefix(o.Key, "chunks/") {
			st.Chunks++
			continue
		}
		k, ok, err := keyFor(ctx, o.Key)
		if err != nil {
			return st, fmt.Errorf("gc: %s: %w", o.Key, err)
		}
		if !ok {
			continue
		}
		st.Manifests++
		g.Go(func() error {
			m, err := s.GetManifest(gctx, k, o.Key)
			if err != nil {
				return fmt.Errorf("gc: %w", err)
			}
			mu.Lock()
			for _, id := range m.Chunks() {
				used[id] = true
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return st, err
	}
	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for _, o := range objs {
		id, ok := strings.CutPrefix(o.Key, "chunks/")
		if !ok || used[id] || !o.Modified.Before(cutoff) {
			continue
		}
		g.Go(func() error {
			err := s.Bucket.DeleteIfMatch(gctx, o.Key, o.ETag)
			if errors.Is(err, bucket.ErrChanged) || errors.Is(err, bucket.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			mu.Lock()
			st.Deleted++
			st.Bytes += o.Size
			mu.Unlock()
			return nil
		})
	}
	return st, g.Wait()
}
