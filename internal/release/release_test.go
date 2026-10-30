package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundle(t *testing.T, names []string, link bool) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for i, name := range names {
		h := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(name)), Typeflag: tar.TypeReg}
		if link && i == 0 {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, "/etc/passwd", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMachine(t *testing.T) {
	files := []string{"hutchd", "hutch-guest", "vmlinux-x86_64", "tmux-x86_64", "terminfo.tar"}
	for _, tt := range []struct {
		name     string
		files    []string
		link     bool
		corrupt  bool
		noSum    bool
		wantFail string
	}{
		{name: "valid", files: files},
		{name: "bad checksum", files: files, corrupt: true, wantFail: "checksum mismatch"},
		{name: "no checksum", files: files, noSum: true, wantFail: "no checksum"},
		{name: "traversal", files: append(append([]string{}, files...), "../outside"), wantFail: "unexpected archive entry"},
		{name: "symlink", files: files, link: true, wantFail: "unexpected archive entry"},
		{name: "missing", files: files[:4], wantFail: "missing terminfo.tar"},
		{name: "duplicate", files: append(append([]string{}, files...), "hutchd"), wantFail: "unexpected archive entry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := bundle(t, tt.files, tt.link)
			sum := fmt.Sprintf("%x", sha256.Sum256(data))
			if tt.corrupt {
				data = append(data, 'x')
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0.1.0/checksums.txt":
					if !tt.noSum {
						fmt.Fprintf(w, "%s  hutch-machine_v0.1.0_linux_amd64.tar.gz\n", sum)
					}
				case "/v0.1.0/hutch-machine_v0.1.0_linux_amd64.tar.gz":
					w.Write(data)
				default:
					t.Errorf("unexpected URL: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := Client{BaseURL: server.URL, HTTP: server.Client()}
			dir, err := c.Machine(context.Background(), "v0.1.0", "amd64")
			if tt.wantFail != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantFail) || dir != "" {
					t.Fatalf("dir=%q err=%v; want %s", dir, err, tt.wantFail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			for _, name := range files {
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(b) != name {
					t.Fatalf("%s: %q, %v", name, b, err)
				}
			}
		})
	}
}

func TestChecksum(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for _, text := range []string{"", "garbage  asset\n", sum + "  asset\n" + sum + "  asset\n"} {
		if _, err := checksum(strings.NewReader(text), "asset"); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestVersion(t *testing.T) {
	for _, v := range []string{"v0.1.0", "v1.2.3-rc.1"} {
		if !IsVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"dev", "06e8587", "v0.1.0-dirty", "v0.1.0-3-g123abc", "v0.1.0-dirty/../../x", "v0.1.0\n", "v0.1.0+build"} {
		if IsVersion(v) {
			t.Fatal(v)
		}
	}
}
