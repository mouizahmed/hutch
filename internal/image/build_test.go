//go:build linux

package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnosticImageCleanup(t *testing.T) {
	b := &Builder{Root: t.TempDir()}
	for _, name := range []string{Default, "project-image", "doctor-../base", "../doctor-escape", "doctor-/../../base"} {
		if err := b.RemoveDiagnostic(name); err == nil {
			t.Fatalf("cleanup accepted %q", name)
		}
	}
	name := "doctor-0123456789abcdef"
	if err := os.MkdirAll(b.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.Dir(name), "rootfs.ext4"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveDiagnostic(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.Dir(name)); !os.IsNotExist(err) {
		t.Fatalf("diagnostic image remains: %v", err)
	}
	if err := b.RemoveDiagnostic(name); err != nil {
		t.Fatalf("cleanup is not idempotent: %v", err)
	}
}
