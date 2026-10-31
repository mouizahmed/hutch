//go:build runtime && linux

package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

// These tests work directly on a fresh machine after its daemon has stopped.
// Exported artifacts contain only synthetic guest state and a disposable key.
func exchangeWorker(t *testing.T) *Worker {
	t.Helper()
	if os.Getenv("HUTCH_TEST_DISPOSABLE") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("requires an explicitly disposable GitHub runner")
	}
	if os.Geteuid() != 0 {
		t.Fatal("runtime exchange requires root")
	}
	version := os.Getenv("HUTCH_TEST_FC")
	if version == "" {
		version = "v1.17.0"
	}
	w, err := New("/var/lib/hutch", version)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual receiving class: %s; warm classes: %v", w.Class(), w.Classes())
	return w
}

func exchangeExec(t *testing.T, w *Worker, id, script string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := w.Exec(t.Context(), id, guestproto.Exec{Argv: []string{"sh", "-eu", "-c", script}}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("guest exec: %d %v: %s", code, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

type exchangeFixture struct {
	Key   durable.Key `json:"synthetic_key"`
	Store Store       `json:"store"`
	Gen   int         `json:"generation"`
	Class string      `json:"class"`
}

func TestSnapshotExport(t *testing.T) {
	w := exchangeWorker(t)
	dir := os.Getenv("HUTCH_TEST_EXCHANGE")
	if !filepath.IsAbs(dir) {
		t.Fatal("HUTCH_TEST_EXCHANGE must be absolute")
	}
	id := "exchange-source"
	t.Cleanup(func() { _ = w.Remove(id) })
	st := &Store{Project: "synthetic", Prefix: "fixture"}
	if _, err := w.Boot(t.Context(), id, "base", 1, 1024, 0, Idle{}, nil, st); err != nil {
		t.Fatal(err)
	}
	exchangeExec(t, w, id, `printf snapshot-exchange > /workspace/exchange-marker
nohup node -e 'let n=0;require("net").createServer(s=>s.on("data",()=>s.end(String(++n)))).listen(19557,"0.0.0.0")' >/tmp/exchange.log 2>&1 </dev/null &
i=0; until ss -ltn | grep -q ':19557'; do i=$((i+1)); test "$i" -lt 100; sleep 0.1; done`)
	if exchangeExec(t, w, id, `node -e 'let s=require("net").connect(19557,"127.0.0.1",()=>s.write("ping"));s.on("data",b=>process.stdout.write(b))'`) != "1" {
		t.Fatal("source process did not initialize")
	}
	b, err := w.Sleep(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	mem := bucket.NewMem()
	s := &durable.Store{Bucket: mem}
	k := durable.NewKey()
	if _, err := w.Upload(t.Context(), id, s, k); err != nil {
		t.Fatal(err)
	}
	objects, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		data, err := mem.Get(t.Context(), obj.Key)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "objects", obj.Key)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(exchangeFixture{Key: k, Store: *st, Gen: b.Generation, Class: b.Class})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("exported actual %s snapshot across machines, %d encrypted objects", b.Class, len(objects))
}

func TestSnapshotImport(t *testing.T) {
	w := exchangeWorker(t)
	root := os.Getenv("HUTCH_TEST_EXCHANGE")
	if !filepath.IsAbs(root) {
		t.Fatal("HUTCH_TEST_EXCHANGE must be absolute")
	}
	fixtures, err := filepath.Glob(filepath.Join(root, "snapshot-*", "fixture.json"))
	if err != nil || len(fixtures) != 3 {
		t.Fatalf("expected current/previous Firecracker and gVisor snapshots, got %d: %v", len(fixtures), err)
	}
	for _, path := range fixtures {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			var f exchangeFixture
			data, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(data, &f) != nil {
				t.Fatal("invalid synthetic fixture")
			}
			mem := bucket.NewMem()
			dir := filepath.Join(filepath.Dir(path), "objects")
			if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				key, err := filepath.Rel(dir, path)
				if err != nil {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				return mem.Put(t.Context(), filepath.ToSlash(key), data, false)
			}); err != nil {
				t.Fatal(err)
			}
			s := &durable.Store{Bucket: mem}
			m, err := s.GetManifest(t.Context(), f.Key, ManifestName(&f.Store, f.Gen))
			if err != nil {
				t.Fatal(err)
			}
			// Corrupt an actual disk chunk. Failure must leave neither a published
			// box nor an incomplete restore tree; repairing it must permit retry.
			var chunk string
			for _, id := range m.Files["rootfs.ext4"].Chunks {
				if id != "" {
					chunk = durable.ChunkKey(id)
					break
				}
			}
			original, err := mem.Get(t.Context(), chunk)
			if err != nil || len(original) == 0 {
				t.Fatal("no encrypted disk chunk")
			}
			broken := bytes.Clone(original)
			broken[len(broken)-1] ^= 1
			if err := mem.Put(t.Context(), chunk, broken, false); err != nil {
				t.Fatal(err)
			}
			id := "exchange-target"
			t.Cleanup(func() { _ = w.Remove(id) })
			if _, err := w.Restore(t.Context(), id, &f.Store, f.Gen, s, f.Key); err == nil {
				t.Fatal("corrupt ciphertext restored successfully")
			}
			for _, name := range []string{id, ".restore-" + id} {
				if _, err := os.Stat(filepath.Join(w.Root, "boxes", name)); !os.IsNotExist(err) {
					t.Fatal("failed restore left a partial or published box")
				}
			}
			if err := mem.Put(t.Context(), chunk, original, false); err != nil {
				t.Fatal(err)
			}
			b, err := w.Restore(t.Context(), id, &f.Store, f.Gen, s, f.Key)
			if err != nil {
				t.Fatal(err)
			}
			warm := w.warm(f.Class)
			if (b.State == Asleep) != warm || (b.State == Archived) == warm {
				t.Fatalf("incorrect warm/cold state for %s -> %s: %s", f.Class, w.Class(), b.State)
			}
			b, err = w.Wake(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if exchangeExec(t, w, id, "cat /workspace/exchange-marker") != "snapshot-exchange" {
				t.Fatal("cross-machine restore lost disk state")
			}
			if warm {
				if exchangeExec(t, w, id, `node -e 'let s=require("net").connect(19557,"127.0.0.1",()=>s.write("ping"));s.on("data",b=>process.stdout.write(b))'`) != "2" {
					t.Fatal("warm restore lost process memory")
				}
			} else if exchangeExec(t, w, id, "if ss -ltn | grep -q ':19557'; then echo old-process-survived; else echo cold; fi") != "cold" {
				t.Fatal("incompatible snapshot was restored warm")
			}
			t.Logf("%s -> %s: warm=%v, disk retained, process state checked, corrupt chunk refused and retry passed", f.Class, b.Class, warm)
		})
	}
}

// Vendor decisions are deterministic even when the runner pool supplies only
// one CPU vendor. Physical cross-vendor claims require both actual vendors.
func TestSnapshotVendorPolicy(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "firecracker", "v1.17.0", "firecracker")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte{}, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, vendor := range []string{"intel", "amd"} {
		w := &Worker{Root: root, FCVersion: "v1.17.0", vendor: vendor}
		for _, other := range []string{"intel", "amd"} {
			peer := &Worker{FCVersion: "v1.17.0", vendor: other}
			if w.warm(peer.Class()) != (vendor == other) {
				t.Fatalf("wrong vendor policy %s -> %s", other, vendor)
			}
		}
	}
}
