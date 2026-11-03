package durable

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
)

type gcUploadBucket struct {
	bucket.Bucket
	afterList, beforeDelete func()
}

func (b gcUploadBucket) List(ctx context.Context, prefix string) ([]bucket.Object, error) {
	objects, err := b.Bucket.List(ctx, prefix)
	if err == nil && b.afterList != nil {
		b.afterList()
	}
	return objects, err
}

func (b gcUploadBucket) DeleteIfMatch(ctx context.Context, key, etag string) error {
	if strings.HasPrefix(key, "chunks/") && b.beforeDelete != nil {
		b.beforeDelete()
	}
	return b.Bucket.DeleteIfMatch(ctx, key, etag)
}

func TestGCConcurrentUpload(t *testing.T) {
	for _, when := range []string{"after list", "before delete"} {
		t.Run(when, func(t *testing.T) {
			ctx := t.Context()
			mem := bucket.NewMem()
			mem.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
			writer := &Store{Bucket: mem, Refresh: 12 * time.Hour}
			key := NewKey()
			const content = "reused chunk"
			if _, _, err := writer.PutFile(ctx, key, strings.NewReader(content), int64(len(content)), nil, nil); err != nil {
				t.Fatal(err)
			}
			mem.Now = nil
			upload := func() {
				file, _, err := writer.PutFile(ctx, key, strings.NewReader(content), int64(len(content)), nil, nil)
				if err == nil {
					err = writer.PutManifest(ctx, key, "m/new", &Manifest{Files: map[string]*File{"file": file}})
				}
				if err != nil {
					t.Error(err)
				}
			}
			b := gcUploadBucket{Bucket: mem}
			if when == "after list" {
				b.afterList = upload
			} else {
				b.beforeDelete = upload
			}
			s := &Store{Bucket: b}
			stats, err := s.GC(ctx, func(_ context.Context, name string) (Key, bool, error) {
				return key, strings.HasPrefix(name, "m/"), nil
			}, 24*time.Hour)
			if err != nil || stats.Deleted != 0 || stats.Bytes != 0 {
				t.Fatalf("sweep: %+v, %v", stats, err)
			}
			manifest, err := writer.GetManifest(ctx, key, "m/new")
			if err != nil {
				t.Fatal(err)
			}
			data, err := writer.GetChunk(ctx, key, manifest.Files["file"].Chunks[0])
			if err != nil || string(data) != content {
				t.Fatalf("new backup lost its chunk: %q, %v", data, err)
			}
		})
	}
}

type ignoresDeleteCondition struct{ bucket.Bucket }

func (b ignoresDeleteCondition) DeleteIfMatch(ctx context.Context, key, _ string) error {
	return b.Delete(ctx, key)
}

func TestGCRefusesUnconditionalEndpoint(t *testing.T) {
	mem := bucket.NewMem()
	mem.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	if err := mem.Put(t.Context(), "chunks/orphan", []byte("keep"), true); err != nil {
		t.Fatal(err)
	}
	s := &Store{Bucket: ignoresDeleteCondition{mem}}
	if _, err := s.GC(t.Context(), func(context.Context, string) (Key, bool, error) {
		return Key{}, false, nil
	}, 24*time.Hour); err == nil {
		t.Fatal("swept with an endpoint that ignores If-Match")
	}
	if _, err := mem.Get(t.Context(), "chunks/orphan"); err != nil {
		t.Fatal("deleted a chunk before checking endpoint support")
	}
}
