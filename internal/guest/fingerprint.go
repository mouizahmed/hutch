//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Fingerprint describes observable build output, not an entire disk image.
// Ignored files, timestamps, process state, and credentials are not inventoried.
// Projects can include other deterministic checks in .hutch/fingerprint.sh.
func Fingerprint(ctx context.Context, root string) (map[string]string, error) {
	out := map[string]string{}
	digest := func(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
	for _, pkg := range []struct {
		name string
		args []string
	}{
		{"dpkg-query", []string{"-W", "-f=${Package}\t${Version}\n"}},
		{"apk", []string{"info", "-v"}},
	} {
		if _, err := exec.LookPath(pkg.name); err != nil {
			continue
		}
		data, err := fingerprintCommand(ctx, root, 2<<20, pkg.name, pkg.args...)
		if err != nil {
			return nil, fmt.Errorf("package inventory: %w", err)
		}
		lines := strings.Split(string(data), "\n")
		slices.Sort(lines)
		out["packages"] = digest([]byte(strings.Join(lines, "\n")))
		break
	}
	// Toolchains a project pins. Agent CLIs install unpinned on every build, so
	// they'd make two fresh builds differ for no fault of the recipe.
	for _, name := range []string{"go", "node", "npm", "python3", "rustc", "cargo"} {
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		arg := "--version"
		if name == "go" {
			arg = "version"
		}
		data, err := fingerprintCommand(ctx, root, 64<<10, name, arg)
		if err != nil {
			return nil, fmt.Errorf("%s version: %w", name, err)
		}
		out["tool "+name] = digest(bytes.TrimSpace(data))
	}
	h := sha256.New()
	if err := fingerprintFiles(ctx, root, h, 0); err != nil {
		return nil, err
	}
	out["working files"] = fmt.Sprintf("%x", h.Sum(nil))
	if _, err := os.Stat(filepath.Join(root, ".hutch", "fingerprint.sh")); err == nil {
		data, err := fingerprintCommand(ctx, root, 2<<20, "bash", "-eo", "pipefail", ".hutch/fingerprint.sh")
		if err != nil {
			return nil, fmt.Errorf(".hutch/fingerprint.sh: %w", err)
		}
		out["project check"] = digest(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

type fingerprintBuffer struct {
	bytes.Buffer
	limit int
}

func (b *fingerprintBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("output limit exceeded")
	}
	return b.Buffer.Write(p)
}

func fingerprintCommand(ctx context.Context, root string, limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out = fingerprintBuffer{limit: limit}
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("command failed, timed out, or exceeded its output limit")
	}
	return out.Bytes(), nil
}

func fingerprintFiles(ctx context.Context, root string, h hash.Hash, depth int) error {
	if depth > 16 {
		return errors.New("submodules exceed fingerprint depth limit")
	}
	data, err := fingerprintCommand(ctx, root, 16<<20, "git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("working-file inventory: %w", err)
	}
	names := strings.Split(string(data), "\x00")
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "" {
			continue
		}
		path := filepath.Join(root, name)
		fi, err := os.Lstat(path)
		fmt.Fprintf(h, "%s\x00", name)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprint(h, "missing\x00")
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00", fi.Mode().Type(), fi.Mode().Perm())
		switch {
		case fi.Mode().IsRegular():
			fmt.Fprintf(h, "%d\x00", fi.Size())
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = copyFingerprint(ctx, h, f)
			f.Close()
			if err != nil {
				return err
			}
		case fi.Mode()&os.ModeSymlink != 0:
			// Hash the link itself, never a host-dependent or missing target.
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprint(h, target)
		case fi.IsDir():
			// A gitlink can be an uninitialized directory. An initialized
			// submodule has its own .git and its working files are checked too.
			if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
				if err := fingerprintFiles(ctx, path, h, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("working-file inventory contains a special file")
		}
		fmt.Fprint(h, "\x00")
	}
	return nil
}

func copyFingerprint(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
