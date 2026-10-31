//go:build linux

package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/durable"
)

func TestImportedGVisorCPURequirements(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "gvisor", "fixture", "runsc")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'sse2,avx,avx2\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Root: root, GVisor: "fixture", vendor: "amd"}
	for _, tt := range []struct {
		name     string
		features []string
		archived bool
		warm     bool
	}{
		{"same features", []string{"sse2", "avx", "avx2"}, false, true},
		{"target superset", []string{"sse2", "avx"}, false, true},
		{"missing AVX512", []string{"sse2", "avx512f"}, false, false},
		{"unknown legacy requirements", nil, false, false},
		{"archived", []string{"sse2"}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := &durable.BoxInfo{GVisor: "fixture", Class: w.Class(), GVisorFeatures: tt.features, Archived: tt.archived}
			got, err := w.restoresWarm(info)
			if err != nil || got != tt.warm {
				t.Fatalf("warm=%v, err=%v; want %v", got, err, tt.warm)
			}
		})
	}
}

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
