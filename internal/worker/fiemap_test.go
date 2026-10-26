//go:build linux

package worker

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/mouizahmed/hutch/internal/durable"
)

// An upload trusts changes() to say which chunks can be skipped: a chunk wrongly
// reported Same is silently lost from every later snapshot.
func TestChangesFindsEveryWrite(t *testing.T) {
	dir := t.TempDir()
	const cs = durable.ChunkSize
	path := filepath.Join(dir, "disk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, cs)
	for _, i := range []int64{0, 1, 3, 4} { // chunk 2 is a hole
		rand.Read(data)
		f.WriteAt(data, i*cs)
	}
	f.Close()
	ref, cur := filepath.Join(dir, "ref"), filepath.Join(dir, "cur")
	if err := clone(path, ref); err != nil {
		t.Skipf("needs a filesystem with reflink (run with TMPDIR on XFS): %v", err)
	}

	check := func(want []durable.Change) {
		t.Helper()
		os.Remove(cur)
		if err := clone(path, cur); err != nil {
			t.Fatal(err)
		}
		c, _ := os.Open(cur)
		r, _ := os.Open(ref)
		defer c.Close()
		defer r.Close()
		change, err := changes(c, r)
		if err != nil {
			t.Fatal(err)
		}
		for i, w := range want {
			if got := change(i); got != w {
				t.Errorf("chunk %d: got %d, want %d", i, got, w)
			}
		}
	}
	check([]durable.Change{durable.Same, durable.Same, durable.Hole, durable.Same, durable.Same})

	// One byte in chunk 1, data in the hole, and a rewrite of chunk 4 with the same bytes.
	f, _ = os.OpenFile(path, os.O_WRONLY, 0)
	f.WriteAt([]byte{1}, cs+12345)
	f.WriteAt([]byte{1}, 2*cs+7)
	same := make([]byte, 4096)
	r, _ := os.Open(path)
	r.ReadAt(same, 4*cs)
	r.Close()
	f.WriteAt(same, 4*cs)
	f.Close()
	check([]durable.Change{durable.Same, durable.Changed, durable.Changed, durable.Same, durable.Changed})
}
