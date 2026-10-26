package durable

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
)

func testFile(t *testing.T, chunks ...byte) (*os.File, []byte) {
	t.Helper()
	var data []byte
	for _, c := range chunks {
		data = append(data, bytes.Repeat([]byte{c}, ChunkSize)...)
	}
	data = append(data, "tail"...)
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	return f, data
}

func TestRoundTripDedupeAndCiphertext(t *testing.T) {
	ctx := context.Background()
	b := bucket.NewMem()
	s := &Store{Bucket: b, Cache: &Cache{Dir: t.TempDir(), Limit: 1 << 30}}
	k := NewKey().ProjectKey("github.com/me/app")
	f, data := testFile(t, 'a', 0, 'a', 'b')

	file, st, err := s.PutFile(ctx, k, f, int64(len(data)), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if file.Chunks[1] != "" || file.Chunks[0] != file.Chunks[2] || st.Uploaded != 3 {
		t.Fatalf("zero chunk or dedupe wrong: %+v %+v", file.Chunks, st)
	}
	objs, _ := b.List(ctx, "")
	for _, o := range objs {
		body, _ := b.Get(ctx, o.Key)
		if bytes.Contains(body, []byte("aaaa")) || bytes.Contains(body, []byte("tail")) {
			t.Fatalf("%s holds plaintext", o.Key)
		}
	}

	// A second put reusing every chunk but the last reads only that one.
	again, st, err := s.PutFile(ctx, k, f, int64(len(data)), file, func(i int) Change {
		if i == len(file.Chunks)-1 {
			return Changed
		}
		return Same
	})
	if err != nil || st.Read != 1 || st.Uploaded != 0 || again.Chunks[4] != file.Chunks[4] {
		t.Fatalf("reuse: %+v %+v %v", again.Chunks, st, err)
	}

	m := &Manifest{Files: map[string]*File{"f": file}}
	if err := s.PutManifest(ctx, k, "projects/x/boxes/b/1", m); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetManifest(ctx, k, "projects/x/boxes/b/1")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.Create(filepath.Join(t.TempDir(), "out"))
	if err := s.GetFile(ctx, k, got.Files["f"], out); err != nil {
		t.Fatal(err)
	}
	back, _ := os.ReadFile(out.Name())
	if !bytes.Equal(back, data) {
		t.Fatal("restored file differs")
	}

	// A chunk swapped for another fails its check.
	body, _ := b.Get(ctx, ChunkKey(file.Chunks[3]))
	b.Put(ctx, ChunkKey(file.Chunks[0]), body, false)
	s.Cache = nil
	if _, err := s.GetChunk(ctx, k, file.Chunks[0]); err == nil {
		t.Fatal("swapped chunk accepted")
	}
}

func TestGC(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	b := bucket.NewMem()
	b.Now = func() time.Time { return now.Add(-48 * time.Hour) }
	s := &Store{Bucket: b, Refresh: 12 * time.Hour}
	k := NewKey()
	f, data := testFile(t, 'a', 'b')

	kept, _, _ := s.PutFile(ctx, k, f, int64(len(data)), nil, nil)
	s.PutManifest(ctx, k, "m/kept", &Manifest{Files: map[string]*File{"f": kept}})
	g, _ := testFile(t, 'c')
	gone, _, _ := s.PutFile(ctx, k, g, ChunkSize, nil, nil)
	b.Now = nil
	h, _ := testFile(t, 'd')
	young, _, _ := s.PutFile(ctx, k, h, ChunkSize, nil, nil)

	keyFor := func(_ context.Context, name string) (Key, bool, error) { return k, strings.HasPrefix(name, "m/"), nil }
	st, err := s.GC(ctx, keyFor, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	exists := func(id string) bool { _, err := b.Head(ctx, ChunkKey(id)); return err == nil }
	for _, id := range kept.Chunks {
		if !exists(id) {
			t.Fatal("deleted a referenced chunk")
		}
	}
	if exists(gone.Chunks[0]) || !exists(young.Chunks[0]) || st.Deleted != 1 {
		t.Fatalf("sweep wrong: %+v", st)
	}

	// Reusing an old unreferenced chunk refreshes it, so a sweep spares it.
	b.Now = func() time.Time { return now.Add(-48 * time.Hour) }
	s.PutFile(ctx, k, g, ChunkSize, nil, nil)
	b.Now = nil
	s.PutFile(ctx, k, g, ChunkSize, nil, nil)
	s.GC(ctx, keyFor, 24*time.Hour)
	if !exists(gone.Chunks[0]) {
		t.Fatal("reused chunk swept before its manifest was written")
	}

	// A manifest that can't be opened stops the sweep.
	b.Put(ctx, "m/foreign", NewKey().Seal("m/foreign", []byte("x")), false)
	if _, err := s.GC(ctx, keyFor, 0); err == nil {
		t.Fatal("swept despite an unreadable manifest")
	}
}
