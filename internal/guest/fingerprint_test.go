//go:build linux

package guest

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorkingFileFingerprint(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func() string {
		t.Helper()
		h := sha256.New()
		if err := fingerprintFiles(t.Context(), root, h, 0); err != nil {
			t.Fatal(err)
		}
		return string(h.Sum(nil))
	}
	git("init", "-q")
	write(".gitignore", "ignored\n")
	write("file with spaces\nand newline", "first")
	git("add", ".")
	write("untracked", "included")
	if err := os.Symlink("missing-target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	first := fingerprint()
	write("ignored", "ignored output")
	if got := fingerprint(); got != first {
		t.Fatal("ignored file changed fingerprint")
	}
	write("untracked", "changed")
	second := fingerprint()
	if second == first {
		t.Fatal("untracked changes were omitted")
	}
	if err := os.Remove(filepath.Join(root, "file with spaces\nand newline")); err != nil {
		t.Fatal(err)
	}
	if got := fingerprint(); got == second {
		t.Fatal("deleted tracked file was omitted")
	}
	third := fingerprint()
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("another-target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if fingerprint() == third {
		t.Fatal("symlink target was omitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if fingerprintFiles(ctx, root, sha256.New(), 0) == nil {
		t.Fatal("ignored cancellation")
	}
}
