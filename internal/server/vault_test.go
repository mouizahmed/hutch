package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
)

type failGetOnce struct {
	bucket.Bucket
	key    string
	failed bool
}

func (b *failGetOnce) Get(ctx context.Context, key string) ([]byte, error) {
	if key == b.key && !b.failed {
		b.failed = true
		return nil, errors.New("temporary chunk read failure")
	}
	return b.Bucket.Get(ctx, key)
}

func TestRestoreRetriesAfterChunkReadFailure(t *testing.T) {
	ctx := context.Background()
	mem := bucket.NewMem()
	st := &durable.Store{Bucket: mem}
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	master := durable.NewKey()
	if err := mem.Put(ctx, "server/master", durable.Wrap(master, "recovery passphrase").Marshal(), true); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "hutch.db")
	created, err := openStore(source, master.Derive("user-layer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := created.ensureWorker("w-saved", "saved"); err != nil {
		created.db.Close()
		t.Fatal(err)
	}
	if err := created.db.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	file, _, err := st.PutFile(ctx, master.ServerKey(), f, info.Size(), nil, nil)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	manifest := &durable.Manifest{Created: createdAt, Files: map[string]*durable.File{"hutch.db": file}}
	name := backupName(1, createdAt)
	if err := st.PutManifest(ctx, master.ServerKey(), name, manifest); err != nil {
		t.Fatal(err)
	}

	data := &failGetOnce{Bucket: mem, key: durable.ChunkKey(file.Chunks[0])}
	cfg := bucket.Config{Name: "test", Region: "us-east-1", AccessKey: "access", SecretKey: "secret"}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreFromBucket(ctx, dir, cfg, "recovery passphrase", "", data); err == nil {
		t.Fatal("first restore unexpectedly succeeded")
	}
	for _, name := range []string{"hutch.db", "master.key", "bucket.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed restore left %s: %v", name, err)
		}
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file changed: %q, %v", got, err)
	}

	if at, err := restoreFromBucket(ctx, dir, cfg, "recovery passphrase", "", data); err != nil || !at.Equal(createdAt) {
		t.Fatalf("retry restore: %v", err)
	}
	recovered, err := openStore(filepath.Join(dir, "hutch.db"), master.Derive("user-layer"))
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.db.Close()
	var gone int
	if err := recovered.db.QueryRow(`SELECT gone FROM workers WHERE id = 'w-saved'`).Scan(&gone); err != nil || gone != 1 {
		t.Fatalf("restored data or recovery changes missing: gone=%d, %v", gone, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file changed after retry: %q, %v", got, err)
	}
}
