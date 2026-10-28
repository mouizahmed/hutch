//go:build linux

package worker

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func page(b byte) []byte { return bytes.Repeat([]byte{b}, pageSize) }

func writePages(t *testing.T, path string, size int, pages map[int][]byte) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(size * pageSize)); err != nil {
		t.Fatal(err)
	}
	for i, p := range pages {
		if _, err := f.WriteAt(p, int64(i*pageSize)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApplyDiff(t *testing.T) {
	dir := t.TempDir()
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Truncate(pageSize)
		err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, pageSize)
		f.Close()
		if err != nil {
			t.Skipf("this file system can't punch holes (gVisor's can't): %v", err)
		}
	}
	base, diff := filepath.Join(dir, "base"), filepath.Join(dir, "diff")
	writePages(t, base, 8, map[int][]byte{0: page(1), 1: page(2), 2: page(3), 5: page(4)})
	// Pages 1-3 changed (2 to zero), page 6 written for the first time; 0 and 5 untouched.
	writePages(t, diff, 8, map[int][]byte{1: page(9), 2: page(0), 3: page(8), 6: page(7)})

	if err := applyDiff(base, diff); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Join([][]byte{page(1), page(9), page(0), page(8), page(0), page(4), page(7), page(0)}, nil)
	if !bytes.Equal(got, want) {
		t.Fatal("merged memory differs")
	}
	if isData(t, base, 2) {
		t.Error("zeroed page 2 is not a hole")
	}

	if err := applyDiff(diff, diff); err != nil {
		t.Fatal(err)
	}
	if isData(t, diff, 2) || !isData(t, diff, 3) {
		t.Error("in place: want only zero pages punched")
	}
}

func isData(t *testing.T, path string, pageN int) bool {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	off, err := unix.Seek(int(f.Fd()), int64(pageN*pageSize), unix.SEEK_DATA)
	return err == nil && off == int64(pageN*pageSize)
}
