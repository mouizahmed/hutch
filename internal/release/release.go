// Package release downloads the machine files matching a released Hutch CLI.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const BaseURL = "https://github.com/mouizahmed/hutch/releases/download"

var tag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
var describe = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// IsVersion excludes git-describe development builds, which have no assets.
func IsVersion(v string) bool {
	return tag.MatchString(v) && !strings.HasSuffix(v, "-dirty") && !describe.MatchString(v)
}

// MachineSHA256 is the SHA-256 of this release's machine bundles, by
// architecture ("amd64=<hex>,…"), built into the CLI by the release workflow. A
// bundle is trusted through the binary already installed, not through a
// checksum file downloaded beside it.
var MachineSHA256 = ""

// Client permits a different source in tests; production uses GitHub over TLS.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Sums are the machine bundles' SHA-256 by architecture.
	Sums map[string]string
}

func NewClient() *Client {
	sums := map[string]string{}
	for _, kv := range strings.Split(MachineSHA256, ",") {
		if arch, sum, ok := strings.Cut(kv, "="); ok {
			sums[arch] = strings.ToLower(sum)
		}
	}
	return &Client{BaseURL: BaseURL, Sums: sums, HTTP: &http.Client{Timeout: 15 * time.Minute,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" || len(via) >= 10 {
				return fmt.Errorf("refusing insecure or excessive release redirects")
			}
			return nil
		}}}
}

func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("User-Agent", "hutch")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// Machine downloads and verifies a bundle before extracting it into a private
// temporary directory. The caller removes the returned directory after upload.
// Only the five known regular files are accepted; links and extra paths fail.
func (c *Client) Machine(ctx context.Context, version, arch string) (dir string, err error) {
	if !IsVersion(version) || arch != "amd64" {
		return "", fmt.Errorf("no machine release for %q, %q", version, arch)
	}
	asset := "hutch-machine_" + version + "_linux_" + arch + ".tar.gz"
	base := strings.TrimRight(c.BaseURL, "/") + "/" + version + "/"
	sum := c.Sums[arch]
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("this hutch has no checksum built in for %s; install hutch from a release, or pass --hutchd and --images from a build", asset)
	}
	dir, err = os.MkdirTemp("", "hutch-release-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
			dir = ""
		}
	}()
	f, err := os.Create(filepath.Join(dir, "bundle.tar.gz"))
	if err != nil {
		return dir, err
	}
	defer f.Close()
	body, err := c.get(ctx, base+asset)
	if err != nil {
		return dir, err
	}
	hash := sha256.New()
	const maxBundle = 1 << 30
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(body, maxBundle+1))
	body.Close()
	if err != nil {
		return dir, err
	}
	if n > maxBundle || hex.EncodeToString(hash.Sum(nil)) != sum {
		return dir, fmt.Errorf("%s: checksum mismatch or bundle too large", asset)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return dir, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return dir, err
	}
	defer gz.Close()
	uname := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	want := map[string]bool{"hutchd": false, "hutch-guest": false, "vmlinux-" + uname: false, "tmux-" + uname: false, "terminfo.tar": false}
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return dir, e
		}
		seen, known := want[h.Name]
		total += h.Size
		if !known || seen || h.Typeflag != tar.TypeReg || h.Size < 0 || total > maxBundle {
			return dir, fmt.Errorf("%s: unexpected archive entry %q", asset, h.Name)
		}
		out, e := os.OpenFile(filepath.Join(dir, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if e != nil {
			return dir, e
		}
		_, e = io.Copy(out, tr)
		cerr := out.Close()
		if e != nil {
			return dir, e
		}
		if cerr != nil {
			return dir, cerr
		}
		want[h.Name] = true
	}
	for name, seen := range want {
		if !seen {
			return dir, fmt.Errorf("%s: missing %s", asset, name)
		}
	}
	return dir, nil
}
