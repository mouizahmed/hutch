//go:build linux

// Package image builds box images on the machine: an OCI image from a Dockerfile
// (BuildKit, with every build step in a gVisor sandbox), Hutch's layer on top, and
// the result flattened into the ext4 disk both backends boot.
package image

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Under Root (/var/lib/hutch):
//
//	images/<name>/{rootfs.ext4,image.json}
//	layer/{hutch-guest,tmux-<arch>,terminfo.tar}   what Hutch adds to every image
//	buildkit/<version>/bin/   buildkit/state/   buildkit/buildkitd.log
//	gvisor/<version>/{runsc,gvisor-bin/}
const Socket = "/run/hutch/buildkit.sock"

// Default is the image projects without a Dockerfile use.
const Default = "base"

//go:embed default.Dockerfile
var defaultDockerfile []byte

// Info describes a built image.
type Info struct {
	Name string `json:"name"`
	// Sources are the images the build started from, pinned by digest.
	Sources []Source  `json:"sources"`
	Built   time.Time `json:"built"`
}

type Source struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func (s Source) String() string { return s.Ref + "@" + s.Digest }

// Builder runs BuildKit for the machine, starting it on first use.
type Builder struct {
	Root string
	// CacheBytes caps BuildKit's layer cache.
	CacheBytes int64
	mu         sync.Mutex
}

func (b *Builder) Dir(name string) string { return filepath.Join(b.Root, "images", name) }

// Get returns a built image's description.
func (b *Builder) Get(name string) (Info, error) {
	var info Info
	data, err := os.ReadFile(filepath.Join(b.Dir(name), "image.json"))
	if errors.Is(err, os.ErrNotExist) {
		return info, fmt.Errorf("no image %q", name)
	}
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(data, &info)
}

// BuildDefault builds Hutch's default image.
func (b *Builder) BuildDefault(ctx context.Context, out io.Writer) (Info, error) {
	return b.Build(ctx, Default, map[string][]byte{"Dockerfile": defaultDockerfile}, out)
}

// FromRef builds an image that is a published image plus Hutch's layer.
func (b *Builder) FromRef(ctx context.Context, name, ref string, out io.Writer) (Info, error) {
	return b.Build(ctx, name, map[string][]byte{"Dockerfile": []byte("FROM " + ref + "\n")}, out)
}

// Build builds the Dockerfile in files (the build context: file name → content)
// into image name, replacing any image of that name.
func (b *Builder) Build(ctx context.Context, name string, files map[string][]byte, out io.Writer) (Info, error) {
	if err := b.ensure(ctx); err != nil {
		return Info{}, fmt.Errorf("start buildkit: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(b.Root, "tmp"), 0o755); err != nil {
		return Info{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Join(b.Root, "tmp"), "build-"+name+"-")
	if err != nil {
		return Info{}, err
	}
	defer os.RemoveAll(tmp)
	bctx, tree := filepath.Join(tmp, "context"), filepath.Join(tmp, "tree")
	for _, d := range []string{bctx, tree} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return Info{}, err
		}
	}
	for n, data := range files {
		p := filepath.Join(bctx, filepath.Clean("/"+n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Info{}, err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return Info{}, err
		}
	}

	// The tar export keeps owners and xattrs; a directory export would not.
	build := exec.CommandContext(ctx, b.bin("buildctl"), "--addr", "unix://"+Socket, "build",
		"--frontend", "dockerfile.v0", "--local", "context="+bctx, "--local", "dockerfile="+bctx,
		"--opt", "attest:provenance=mode=min", "--output", "type=tar,dest=-", "--progress", "plain")
	untar := exec.CommandContext(ctx, "tar", "-x", "-p", "--numeric-owner", "--xattrs", "--xattrs-include=*", "-f", "-", "-C", tree)
	pipe, err := build.StdoutPipe()
	if err != nil {
		return Info{}, err
	}
	var tarErr bytes.Buffer
	build.Stderr, untar.Stdin, untar.Stderr = out, pipe, &tarErr
	if err := untar.Start(); err != nil {
		return Info{}, err
	}
	if err := build.Run(); err != nil {
		untar.Wait()
		return Info{}, fmt.Errorf("build failed: %w", err)
	}
	if err := untar.Wait(); err != nil {
		return Info{}, fmt.Errorf("unpack: %v: %s", err, bytes.TrimSpace(tarErr.Bytes()))
	}

	info := Info{Name: name, Built: time.Now().UTC()}
	if info.Sources, err = provenance(filepath.Join(tree, "provenance.json")); err != nil {
		return Info{}, err
	}
	os.Remove(filepath.Join(tree, "provenance.json"))
	fmt.Fprintln(out, "\n==> Hutch's layer")
	if err := applyLayer(tree, filepath.Join(b.Root, "layer")); err != nil {
		return Info{}, fmt.Errorf("hutch layer: %w", err)
	}
	fmt.Fprintln(out, "==> disk")
	dir := b.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Info{}, err
	}
	if err := flatten(tree, filepath.Join(dir, "rootfs.ext4")); err != nil {
		return Info{}, err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return Info{}, err
	}
	return info, os.WriteFile(filepath.Join(dir, "image.json"), append(data, '\n'), 0o644)
}

// provenance reads the build's base images from BuildKit's provenance attestation.
func provenance(path string) ([]Source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("provenance: %w", err)
	}
	// SLSA v1, as BuildKit writes it.
	var p struct {
		Predicate struct {
			BuildDefinition struct {
				ResolvedDependencies []struct {
					URI    string            `json:"uri"`
					Digest map[string]string `json:"digest"`
				} `json:"resolvedDependencies"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("provenance: %w", err)
	}
	var out []Source
	for _, m := range p.Predicate.BuildDefinition.ResolvedDependencies {
		ref, ok := strings.CutPrefix(m.URI, "pkg:docker/")
		if !ok || m.Digest["sha256"] == "" {
			continue
		}
		ref, _, _ = strings.Cut(ref, "?")
		out = append(out, Source{Ref: ref, Digest: "sha256:" + m.Digest["sha256"]})
	}
	return out, nil
}

// flatten makes an ext4 disk holding tree, with room to spare; boxes grow it to
// their disk size.
func flatten(tree, disk string) error {
	used, err := duBytes(tree)
	if err != nil {
		return err
	}
	size := used*5/4 + 1<<30
	tmp := disk + ".tmp"
	os.Remove(tmp)
	if out, err := exec.Command("mkfs.ext4", "-q", "-L", "rootfs", "-d", tree, tmp, fmt.Sprintf("%dk", size>>10)).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return os.Rename(tmp, disk)
}

func duBytes(dir string) (int64, error) {
	out, err := exec.Command("du", "-sb", dir).Output()
	if err != nil {
		return 0, err
	}
	var n int64
	_, err = fmt.Sscan(string(out), &n)
	return n, err
}

// newest returns the highest-sorting version directory under dir.
func newest(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "state" {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("nothing installed in %s: run hutch setup again", dir)
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}

func (b *Builder) bin(name string) string {
	dir, err := newest(filepath.Join(b.Root, "buildkit"))
	if err != nil {
		return name
	}
	return filepath.Join(dir, "bin", name)
}

// Runsc is the newest installed gVisor runtime.
func Runsc(root string) (string, error) {
	dir, err := newest(filepath.Join(root, "gvisor"))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "runsc"), nil
}

// shim lets BuildKit, which drives runc, run each step in gVisor: runsc has no
// --keep, its root overlay must be off for BuildKit to see a step's changes, and
// it uses the step's network namespace as BuildKit set it up.
const shim = `#!/bin/sh
for a; do shift; [ "$a" = --keep ] || set -- "$@" "$a"; done
exec %s --overlay2=none --network=host "$@"
`

// buildNet is BuildKit's bridge network; build steps reach the internet only.
const buildNet = "10.10.0.0/16"

// ensure starts buildkitd unless it's answering.
func (b *Builder) ensure(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, err := net.Dial("unix", Socket); err == nil {
		c.Close()
		return nil
	}
	runsc, err := Runsc(b.Root)
	if err != nil {
		return err
	}
	bk, err := newest(filepath.Join(b.Root, "buildkit"))
	if err != nil {
		return err
	}
	state := filepath.Join(b.Root, "buildkit", "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	shimPath := filepath.Join(state, "runsc-shim")
	if err := os.WriteFile(shimPath, []byte(fmt.Sprintf(shim, runsc)), 0o755); err != nil {
		return err
	}
	conf := filepath.Join(state, "buildkitd.toml")
	cache := b.CacheBytes >> 20
	toml := fmt.Sprintf("[dns]\n  nameservers = [\"1.1.1.1\", \"8.8.8.8\"]\n\n[worker.oci]\n  gc = true\n  reservedSpace = \"%dMB\"\n  maxUsedSpace = \"%dMB\"\n", cache, cache)
	if err := os.WriteFile(conf, []byte(toml), 0o644); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(b.Root, "buildkit", "buildkitd.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	os.MkdirAll(filepath.Dir(Socket), 0o755)
	cmd := exec.Command(filepath.Join(bk, "bin", "buildkitd"), "--config", conf, "--root", state, "--addr", "unix://"+Socket,
		"--oci-worker-binary", shimPath, "--oci-worker-net", "bridge", "--containerd-worker=false")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(bk, "bin")+":/usr/sbin:/usr/bin:/sbin:/bin")
	cmd.Stdout, cmd.Stderr = logf, logf
	// It stops with hutchd, and starts again on the next build.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if c, err := net.Dial("unix", Socket); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			return fmt.Errorf("buildkitd did not start; see %s", logf.Name())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return firewall()
}

// Private ranges a build step must never reach: the machine's LAN, other private
// networks, link-local (cloud metadata), and the machine itself.
var blocked = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

func firewall() error {
	rules := [][]string{{"INPUT", "-s", buildNet, "-m", "conntrack", "--ctstate", "NEW", "-j", "DROP"}}
	for _, c := range blocked {
		rules = append(rules, []string{"FORWARD", "-s", buildNet, "-d", c, "-j", "DROP"})
	}
	for _, r := range rules {
		if exec.Command("iptables", append([]string{"-C"}, r...)...).Run() == nil {
			continue
		}
		if out, err := exec.Command("iptables", append([]string{"-I"}, r...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables: %v: %s", err, out)
		}
	}
	return nil
}
