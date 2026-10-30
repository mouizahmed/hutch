//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/worker"
)

const xfsMagic = 0x58465342

// doctor checks what a worker needs to run boxes.
func doctor(w *worker.Worker) []api.Check {
	var out []api.Check
	add := func(name string, ok bool, detail, fix string) {
		c := api.Check{Area: "machine", Name: name, Status: "ok", Detail: detail}
		if !ok {
			c.Status, c.Fix = "fail", fix
		}
		out = append(out, c)
	}
	if w.GVisor != "" {
		add("backend", true, "gVisor sandboxes (no /dev/kvm): builds about 1.5× slower than a VM", "")
	} else {
		add("backend", true, "Firecracker VMs (/dev/kvm)", "")
	}

	var u unix.Utsname
	unix.Uname(&u)
	release := unix.ByteSliceToString(u.Release[:])
	major, minor := 0, 0
	if parts := strings.SplitN(release, ".", 3); len(parts) >= 2 {
		major, _ = strconv.Atoi(parts[0])
		minor, _ = strconv.Atoi(parts[1])
	}
	add("kernel", major > 6 || (major == 6 && minor >= 1), release, "upgrade the host kernel to 6.1 or newer")

	var st syscall.Statfs_t
	statErr := syscall.Statfs(w.Root, &st)
	reflink := false
	if statErr == nil && st.Type == xfsMagic {
		reflink = checkReflink(w.Root)
	}
	add("XFS with reflink", reflink, w.Root, fmt.Sprintf("put %s on XFS made with mkfs.xfs -m reflink=1", w.Root))

	err := pressureEvents()
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	add("memory pressure events", err == nil, detail, "boot with psi=1 (or a kernel with CONFIG_PSI), so overcommit reacts at once rather than at the next check")

	classes := w.Classes()
	if w.GVisor != "" {
		add("gVisor", len(classes) > 0, strings.Join(classes, ", "), "run hutch setup again")
	} else {
		add("Firecracker", len(classes) > 0, strings.Join(classes, ", "), "install a Firecracker release under "+filepath.Join(w.Root, "firecracker"))
	}
	return out
}

// checkReflink uses its own temporary directory, so concurrent doctors do not
// overwrite each other's probes or follow preexisting filenames.
func checkReflink(root string) bool {
	dir, err := os.MkdirTemp(root, ".doctor-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	a := filepath.Join(dir, "source")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		return false
	}
	src, err := os.Open(a)
	if err != nil {
		return false
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "clone"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	defer dst.Close()
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}
