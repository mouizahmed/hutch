//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxIDsAndTemplateRename(t *testing.T) {
	// OCI's abbreviation lookup cannot confuse prefix-related human names.
	parent, child := sandboxID("box"), sandboxID("box-child")
	if parent == child || strings.HasPrefix(parent, child) || strings.HasPrefix(child, parent) || parent[0] != 'H' {
		t.Fatalf("unsafe runtime IDs: %q %q", parent, child)
	}
	w := &Worker{Root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(w.Root, "boxes", "template"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Box{ID: "template", State: Asleep, RunscID: sandboxID("template")}
	if err := w.save(b); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Rename("template", "version", nil); err != nil {
		t.Fatal(err)
	}
	version, err := w.load("version")
	if err != nil {
		t.Fatal(err)
	}
	if version.RunscID != "" {
		t.Fatal("renamed version retained the runtime ID of the next template box")
	}
	if legacy := (&Box{ID: "legacy"}).sandboxID(); legacy != "legacy" {
		t.Fatal("an existing running legacy sandbox no longer addresses its old OCI ID")
	}
}
