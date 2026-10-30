//go:build linux

package image

import (
	"archive/tar"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

//go:embed tmux.conf
var tmuxConf []byte

// devUID is the dev user's uid and gid in every box.
const devUID = 1000

// applyLayer adds Hutch's layer to an image's tree: hutch-guest (the box's init),
// tmux when the image has none, terminfo it lacks, the dev user, and the files a
// box's identity and network need.
func applyLayer(tree, layer string) error {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := normalizeImageLinks(root); err != nil {
		return err
	}
	in := imagePath
	if err := copyFile(root, filepath.Join(layer, "hutch-guest"), in("usr/local/bin/hutch-guest"), 0o755); err != nil {
		return err
	}
	if !exists(root, in("usr/bin/tmux")) && !exists(root, in("usr/local/bin/tmux")) && !exists(root, in("bin/tmux")) {
		if err := copyFile(root, filepath.Join(layer, "tmux-"+arch()), in("usr/local/bin/tmux"), 0o755); err != nil {
			return err
		}
	}
	if err := addTerminfo(root, filepath.Join(layer, "terminfo.tar")); err != nil {
		return err
	}
	if err := writeFile(root, in("etc/tmux.conf"), tmuxConf, 0o644); err != nil {
		return err
	}
	if err := devUser(root); err != nil {
		return err
	}
	for p, content := range map[string]string{
		"etc/hostname":    "hutch\n",
		"etc/hosts":       "127.0.0.1 localhost\n127.0.1.1 hutch\n",
		"etc/resolv.conf": "nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
		"etc/machine-id":  "",
	} {
		root.Remove(in(p)) // often a symlink into /run
		if err := writeFile(root, in(p), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := root.MkdirAll(in("etc/hutch"), 0o755); err != nil {
		return err
	}
	if err := root.MkdirAll(in("workspace"), 0o755); err != nil {
		return err
	}
	if err := root.Chown(in("workspace"), devUID, devUID); err != nil {
		return err
	}
	// Host keys are made per box (hutch-guest replaces them in every clone); RSA is
	// left out, as making it takes about a second on every fork.
	if exists(root, in("usr/sbin/sshd")) {
		keys, err := fs.Glob(root.FS(), "etc/ssh/ssh_host_*")
		if err != nil {
			return err
		}
		for _, k := range keys {
			root.Remove(k)
		}
		if exists(root, in("etc/ssh/sshd_config.d")) {
			conf := "HostKey /etc/ssh/ssh_host_ed25519_key\nHostKey /etc/ssh/ssh_host_ecdsa_key\n"
			if err := writeFile(root, in("etc/ssh/sshd_config.d/hutch.conf"), []byte(conf), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func arch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// addTerminfo installs the layer's terminfo entries the image doesn't have.
func addTerminfo(root *os.Root, archive string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if !filepath.IsLocal(h.Name) {
			return fmt.Errorf("invalid terminfo path %q", h.Name)
		}
		name := filepath.Clean(h.Name)
		have := false
		for _, dir := range []string{"etc/terminfo", "lib/terminfo", "usr/share/terminfo"} {
			if exists(root, filepath.Join(dir, name)) {
				have = true
			}
		}
		if have {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return errors.New("terminfo entry exceeds 1 MiB")
		}
		if err := writeFile(root, filepath.Join("usr/share/terminfo", name), data, 0o644); err != nil {
			return err
		}
	}
}

// devUser makes uid 1000 the dev user with passwordless sudo. An image's own
// uid-1000 user (ubuntu, node) becomes dev, home and all; a dev user with another
// uid can't be.
func devUser(root *os.Root) error {
	in := imagePath
	passwd, err := readLines(root, in("etc/passwd"))
	if err != nil {
		return err
	}
	shell := "/bin/sh"
	if exists(root, in("bin/bash")) {
		shell = "/bin/bash"
	}
	old, home := "", "/home/dev"
	for i, l := range passwd {
		f := strings.Split(l, ":")
		if len(f) < 7 {
			continue
		}
		if f[0] == "dev" && f[2] != fmt.Sprint(devUID) {
			return fmt.Errorf("the image has a dev user with uid %s; Hutch's dev user is uid %d", f[2], devUID)
		}
		if f[2] == fmt.Sprint(devUID) {
			old = f[0]
			if f[5] == "/home/"+old {
				f[5] = home
			}
			home = f[5]
			f[0], f[3], f[6] = "dev", fmt.Sprint(devUID), shell
			passwd[i] = strings.Join(f, ":")
		}
	}
	if old == "" {
		passwd = append(passwd, fmt.Sprintf("dev:x:%d:%d::%s:%s", devUID, devUID, home, shell))
	}
	if err := writeLines(root, in("etc/passwd"), passwd, 0o644); err != nil {
		return err
	}
	if old != "" && old != "dev" && exists(root, in("home/"+old)) && !exists(root, in(home)) {
		if err := root.Rename(in("home/"+old), in(home)); err != nil {
			return err
		}
	}

	// The group with gid 1000 becomes dev's; memberships follow the rename.
	group, err := readLines(root, in("etc/group"))
	if err != nil {
		return err
	}
	haveGroup := false
	for i, l := range group {
		f := strings.Split(l, ":")
		if len(f) < 4 {
			continue
		}
		if f[2] == fmt.Sprint(devUID) {
			f[0], haveGroup = "dev", true
		} else if f[0] == "dev" {
			return fmt.Errorf("the image has a dev group with gid %s; Hutch's dev group is gid %d", f[2], devUID)
		}
		if old != "" {
			members := strings.Split(f[3], ",")
			for j, m := range members {
				if m == old {
					members[j] = "dev"
				}
			}
			f[3] = strings.Join(members, ",")
		}
		group[i] = strings.Join(f, ":")
	}
	if !haveGroup {
		group = append(group, fmt.Sprintf("dev:x:%d:", devUID))
	}
	if err := writeLines(root, in("etc/group"), group, 0o644); err != nil {
		return err
	}
	for _, p := range []string{"etc/shadow", "etc/gshadow"} {
		lines, err := readLines(root, in(p))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		found := false
		for i, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			f := strings.SplitN(l, ":", 2)
			if len(f) < 2 {
				return fmt.Errorf("malformed %s entry", p)
			}
			if old != "" && f[0] == old || f[0] == "dev" {
				lines[i], found = "dev:"+f[1], true
			}
		}
		if !found {
			entry := "dev:!:19000:0:99999:7:::"
			if p == "etc/gshadow" {
				entry = "dev:!::"
			}
			lines = append(lines, entry)
		}
		if err := writeLines(root, in(p), lines, 0o640); err != nil {
			return err
		}
	}

	if !exists(root, in(home)) {
		if err := root.MkdirAll(in(home), 0o750); err != nil {
			return err
		}
		if skel := in("etc/skel"); exists(root, skel) {
			entries, _ := fs.ReadDir(root.FS(), skel)
			for _, e := range entries {
				if e.Type().IsRegular() {
					data, err := readImageFile(root, filepath.Join(skel, e.Name()))
					if err == nil {
						writeFile(root, filepath.Join(in(home), e.Name()), data, 0o644)
					}
				}
			}
		}
	}
	if err := chownTree(root, in(home), devUID); err != nil {
		return err
	}
	if exists(root, in("etc/sudoers.d")) {
		return writeFile(root, in("etc/sudoers.d/dev"), []byte("dev ALL=(ALL) NOPASSWD:ALL\n"), 0o440)
	}
	return nil
}

// imagePath addresses absolute guest paths relative to the opened image root.
func imagePath(p string) string { return strings.TrimPrefix(p, "/") }

// Absolute image links refer to the guest root, never the machine's root. Turn
// them into equivalent relative links before using os.Root. Links that still
// escape the tree are refused by os.Root on every subsequent operation.
func normalizeImageLinks(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := root.Readlink(name)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(target) {
			return nil
		}
		rel, err := filepath.Rel("/"+filepath.Dir(name), filepath.Clean(target))
		if err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return err
		}
		return root.Symlink(rel, name)
	})
}

func chownTree(root *os.Root, dir string, id int) error {
	return fs.WalkDir(root.FS(), dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(p, id, id)
	})
}

func exists(root *os.Root, p string) bool {
	_, err := root.Lstat(p)
	return err == nil
}

func readImageFile(root *os.Root, p string) ([]byte, error) {
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular image file", p)
	}
	// These are account/configuration files, not arbitrary image contents.
	if fi.Size() > 4<<20 {
		return nil, fmt.Errorf("%s exceeds 4 MiB", p)
	}
	return io.ReadAll(io.LimitReader(f, (4<<20)+1))
}

func readLines(root *os.Root, p string) ([]string, error) {
	data, err := readImageFile(root, p)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n"), nil
}

func writeLines(root *os.Root, p string, lines []string, mode os.FileMode) error {
	return writeFile(root, p, []byte(strings.Join(lines, "\n")+"\n"), mode)
}

// writeFile replaces p, keeping its owner when it existed. Every filesystem
// operation stays under the opened image root, including temporary files.
func writeFile(root *os.Root, p string, data []byte, mode os.FileMode) error {
	if err := root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	uid, gid := os.Geteuid(), os.Getegid()
	if fi, err := root.Stat(p); err == nil {
		if st, ok := statOwner(fi); ok {
			uid, gid = st[0], st[1]
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.hutch-%x", p, nonce)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Chown(uid, gid); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, p)
}

func copyFile(root *os.Root, src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := root.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFile(root, dst, data, mode)
}

func statOwner(fi os.FileInfo) ([2]int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int{}, false
	}
	return [2]int{int(st.Uid), int(st.Gid)}, true
}
