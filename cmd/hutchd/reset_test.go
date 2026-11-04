//go:build linux

package main

import (
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReset(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "api.sock")
	for _, name := range []string{"boxes/scratch/box.json", "state/master.key", "link/lead", "worker-id", "fenced", "no-boxes", "cache/chunks/data", "images/base/image.json", "buildkit/state/cache", "layer/hutch-guest"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte("fixture")
		if strings.HasSuffix(name, "box.json") {
			data = []byte(`{"id":"scratch","state":"asleep"}`)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := reset(root, socket, nil); err == nil {
		t.Fatal("reset without --yes succeeded")
	}
	if err := reset(root, socket, []string{"--check"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "state/master.key")); err != nil {
		t.Fatal("preflight deleted setup state:", err)
	}
	if err := reset(root, socket, []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"boxes", "state", "link", "worker-id", "fenced", "no-boxes", "cache"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s remains: %v", name, err)
		}
	}
	for _, name := range []string{"images/base/image.json", "buildkit/state/cache", "layer/hutch-guest"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("installation file %s was removed: %v", name, err)
		}
	}
}

func TestResetRefusesRunningDaemon(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "api.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := reset(root, socket, []string{"--yes"}); err == nil || !strings.Contains(err.Error(), "hutchd is running") {
		t.Fatalf("reset with a running daemon: %v", err)
	}
}

func TestResetRefusesLeadWithWorkers(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "state/hutch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE workers (name TEXT, linked INTEGER, gone INTEGER);
		INSERT INTO workers VALUES ('local', 0, 0), ('worker', 1, 0), ('gone', 1, 1);`); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--check"}, {"--yes"}} {
		if err := reset(root, filepath.Join(root, "api.sock"), args); err == nil || !strings.Contains(err.Error(), "still has workers (worker)") {
			t.Fatalf("reset %v: %v", args, err)
		}
	}
	if _, err := db.Exec(`UPDATE workers SET gone = 1 WHERE linked = 1`); err != nil {
		t.Fatal(err)
	}
	if err := reset(root, filepath.Join(root, "api.sock"), []string{"--check"}); err != nil {
		t.Fatal(err)
	}
}
