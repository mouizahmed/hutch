//go:build linux

package image

import (
	"archive/tar"
	_ "embed"
	"errors"
	"fmt"
	"io"
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
	in := func(p string) string { return filepath.Join(tree, p) }
	if err := copyFile(filepath.Join(layer, "hutch-guest"), in("usr/local/bin/hutch-guest"), 0o755); err != nil {
		return err
	}
	if !exists(in("usr/bin/tmux")) && !exists(in("usr/local/bin/tmux")) && !exists(in("bin/tmux")) {
		if err := copyFile(filepath.Join(layer, "tmux-"+arch()), in("usr/local/bin/tmux"), 0o755); err != nil {
			return err
		}
	}
	if err := addTerminfo(tree, filepath.Join(layer, "terminfo.tar")); err != nil {
		return err
	}
	if err := writeFile(in("etc/tmux.conf"), tmuxConf, 0o644); err != nil {
		return err
	}
	if err := devUser(tree); err != nil {
		return err
	}
	for p, content := range map[string]string{
		"etc/hostname":    "hutch\n",
		"etc/hosts":       "127.0.0.1 localhost\n127.0.1.1 hutch\n",
		"etc/resolv.conf": "nameserver 1.1.1.1\nnameserver 8.8.8.8\n",
		"etc/machine-id":  "",
	} {
		os.Remove(in(p)) // often a symlink into /run
		if err := writeFile(in(p), []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(in("etc/hutch"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(in("workspace"), 0o755); err != nil {
		return err
	}
	if err := os.Chown(in("workspace"), devUID, devUID); err != nil {
		return err
	}
	// Host keys are made per box (hutch-guest replaces them in every clone); RSA is
	// left out, as making it takes about a second on every fork.
	if exists(in("usr/sbin/sshd")) {
		keys, _ := filepath.Glob(in("etc/ssh/ssh_host_*"))
		for _, k := range keys {
			os.Remove(k)
		}
		if exists(in("etc/ssh/sshd_config.d")) {
			conf := "HostKey /etc/ssh/ssh_host_ed25519_key\nHostKey /etc/ssh/ssh_host_ecdsa_key\n"
			if err := writeFile(in("etc/ssh/sshd_config.d/hutch.conf"), []byte(conf), 0o644); err != nil {
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
func addTerminfo(tree, archive string) error {
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
		name := filepath.Clean("/" + h.Name)[1:]
		have := false
		for _, dir := range []string{"etc/terminfo", "lib/terminfo", "usr/share/terminfo"} {
			if exists(filepath.Join(tree, dir, name)) {
				have = true
			}
		}
		if have {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(tree, "usr/share/terminfo", name), data, 0o644); err != nil {
			return err
		}
	}
}

// devUser makes uid 1000 the dev user with passwordless sudo. An image's own
// uid-1000 user (ubuntu, node) becomes dev, home and all; a dev user with another
// uid can't be.
func devUser(tree string) error {
	in := func(p string) string { return filepath.Join(tree, p) }
	passwd, err := readLines(in("etc/passwd"))
	if err != nil {
		return err
	}
	shell := "/bin/sh"
	if exists(in("bin/bash")) {
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
	if err := writeLines(in("etc/passwd"), passwd, 0o644); err != nil {
		return err
	}
	if old != "" && old != "dev" && exists(in("home/"+old)) && !exists(in(home)) {
		if err := os.Rename(in("home/"+old), in(home)); err != nil {
			return err
		}
	}

	// The group with gid 1000 becomes dev's; memberships follow the rename.
	group, err := readLines(in("etc/group"))
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
	if err := writeLines(in("etc/group"), group, 0o644); err != nil {
		return err
	}
	for _, p := range []string{"etc/shadow", "etc/gshadow"} {
		lines, err := readLines(in(p))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		found := false
		for i, l := range lines {
			f := strings.SplitN(l, ":", 2)
			if f[0] == old || f[0] == "dev" {
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
		if err := writeLines(in(p), lines, 0o640); err != nil {
			return err
		}
	}

	if !exists(in(home)) {
		if err := os.MkdirAll(in(home), 0o750); err != nil {
			return err
		}
		if skel := in("etc/skel"); exists(skel) {
			entries, _ := os.ReadDir(skel)
			for _, e := range entries {
				if e.Type().IsRegular() {
					data, err := os.ReadFile(filepath.Join(skel, e.Name()))
					if err == nil {
						writeFile(filepath.Join(in(home), e.Name()), data, 0o644)
					}
				}
			}
		}
	}
	if err := chownTree(in(home), devUID); err != nil {
		return err
	}
	if exists(in("etc/sudoers.d")) {
		return writeFile(in("etc/sudoers.d/dev"), []byte("dev ALL=(ALL) NOPASSWD:ALL\n"), 0o440)
	}
	return nil
}

func chownTree(dir string, id int) error {
	return filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, id, id)
	})
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func readLines(p string) ([]string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n"), nil
}

func writeLines(p string, lines []string, mode os.FileMode) error {
	return writeFile(p, []byte(strings.Join(lines, "\n")+"\n"), mode)
}

// writeFile replaces p, keeping its owner when it existed.
func writeFile(p string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	uid, gid := 0, 0
	if fi, err := os.Stat(p); err == nil {
		if st, ok := statOwner(fi); ok {
			uid, gid = st[0], st[1]
		}
	}
	tmp := p + ".hutch-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Lchown(tmp, uid, gid); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	os.Remove(dst)
	return writeFile(dst, data, mode)
}

func statOwner(fi os.FileInfo) ([2]int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int{}, false
	}
	return [2]int{int(st.Uid), int(st.Gid)}, true
}
