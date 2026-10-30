//go:build linux

package image

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLayerCannotFollowImageLinkOutsideTree(t *testing.T) {
	tree, layer, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "usr", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "usr", "local", "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "hutch-guest"), []byte("outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layer, "hutch-guest"), []byte("guest fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	applyLayer(tree, layer) // An incomplete layer may fail, but must stay in tree.
	data, err := os.ReadFile(filepath.Join(outside, "hutch-guest"))
	if err != nil || string(data) != "outside sentinel" {
		t.Fatalf("image link changed a file outside the tree: %q, %v", data, err)
	}
}

func TestRootedImagePaths(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.MkdirAll("usr/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("/usr/bin", "bin"); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := root.Lchown("bin", 1000, 1000); err != nil {
			t.Fatal(err)
		}
	}
	before, err := root.Lstat("bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := normalizeImageLinks(root); err != nil {
		t.Fatal(err)
	}
	after, err := root.Lstat("bin")
	if err != nil {
		t.Fatal(err)
	}
	wantOwner, _ := statOwner(before)
	gotOwner, _ := statOwner(after)
	if gotOwner != wantOwner {
		t.Fatalf("guest link owner changed: %v -> %v", wantOwner, gotOwner)
	}
	if err := writeFile(root, "bin/tool", []byte("in image"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := root.ReadFile("usr/bin/tool"); err != nil || string(data) != "in image" {
		t.Fatalf("absolute guest link did not resolve inside image: %q, %v", data, err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "escape/blocked", []byte("outside"), 0o600); err == nil {
		t.Fatal("followed an escaping parent symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "blocked")); !os.IsNotExist(err) {
		t.Fatalf("created outside file: %v", err)
	}
	if err := root.Symlink("/outside", "usr/bin/target.hutch-tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "usr/bin/target", []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("usr/bin/target.hutch-tmp"); err != nil {
		t.Fatal("used a preexisting temporary path")
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageFile(root, "fifo"); err == nil {
		t.Fatal("accepted a special image file")
	}
}
