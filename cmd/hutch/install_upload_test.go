//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// A child helper substitutes only the remote zstd executable. The real shell,
// mktemp, checksum, chmod, and rename commands exercise the upload transaction.
func TestUploadHelperProcess(t *testing.T) {
	if os.Getenv("HUTCH_UPLOAD_HELPER") != "zstd" {
		return
	}
	if os.Getenv("HUTCH_UPLOAD_MODE") == "fail" {
		os.Exit(9)
	}
	dec, err := zstd.NewReader(os.Stdin)
	if err != nil {
		os.Exit(10)
	}
	data, err := io.ReadAll(dec)
	dec.Close()
	if err != nil {
		os.Exit(11)
	}
	if os.Getenv("HUTCH_UPLOAD_MODE") == "corrupt" && len(data) > 0 {
		data[0] ^= 0xff
	}
	if _, err := os.Stdout.Write(data); err != nil {
		os.Exit(12)
	}
	os.Exit(0)
}

func uploadFixture(t *testing.T) remote {
	t.Helper()
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUTCH_UPLOAD_TEST_BINARY", exe)
	t.Setenv("HUTCH_UPLOAD_MODE", "")
	for name, script := range map[string]string{
		"ssh":  "#!/bin/sh\nfor arg; do last=$arg; done\nexec sh -c \"$last\"\n",
		"sudo": "#!/bin/sh\nexec \"$@\"\n",
		"zstd": "#!/bin/sh\nHUTCH_UPLOAD_HELPER=zstd exec \"$HUTCH_UPLOAD_TEST_BINARY\" -test.run=^TestUploadHelperProcess$\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return remote{host: "upload-fixture"}
}

func TestRemoteUploadAtomicity(t *testing.T) {
	r := uploadFixture(t)
	dir := filepath.Join(t.TempDir(), "directory with spaces and 'quote")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "hutchd")
	payload := bytes.Repeat([]byte("verified executable payload\n"), 10000)
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "fail", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HUTCH_UPLOAD_MODE", mode)
			if err := os.WriteFile(dst, []byte("previous executable"), 0o700); err != nil {
				t.Fatal(err)
			}
			err := r.upload(src, dst, 0o751)
			got, readErr := os.ReadFile(dst)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "" {
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("upload failed: %v", err)
				}
				fi, _ := os.Stat(dst)
				if fi.Mode().Perm() != 0o751 {
					t.Fatalf("incorrect installed mode: %v", fi.Mode())
				}
			} else {
				if err == nil || string(got) != "previous executable" {
					t.Fatalf("failed upload replaced the installed executable: %v", err)
				}
				if mode == "corrupt" && !strings.Contains(err.Error(), "checksum verification") {
					t.Fatalf("missing checksum failure: %v", err)
				}
			}
			left, _ := filepath.Glob(filepath.Join(dir, ".hutch-upload.*"))
			if len(left) != 0 {
				t.Fatalf("upload leaked temporary files: %v", left)
			}
		})
	}
}

func TestConcurrentRemoteUploadsPublishCompleteFiles(t *testing.T) {
	r := uploadFixture(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "hutchd")
	var payloads [][]byte
	var sources []string
	for i := range 6 {
		data := bytes.Repeat([]byte(fmt.Sprintf("payload %d\n", i)), 10000)
		path := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		payloads, sources = append(payloads, data), append(sources, path)
	}
	var wg sync.WaitGroup
	errors := make(chan error, len(sources))
	for _, src := range sources {
		wg.Go(func() { errors <- r.upload(src, dst, 0o755) })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range payloads {
		if bytes.Equal(got, want) {
			return
		}
	}
	t.Fatal("concurrent transfers published mixed or incomplete file contents")
}
