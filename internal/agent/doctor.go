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
	_, err := os.Stat("/dev/kvm")
	add("/dev/kvm", err == nil, "", "enable virtualization: a dedicated server or a VPS plan with nested virtualization; on your own hardware, VT-x or AMD-V in firmware")

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
	syscall.Statfs(w.Root, &st)
	reflink := false
	if st.Type == xfsMagic {
		a, b := filepath.Join(w.Root, ".doctor-a"), filepath.Join(w.Root, ".doctor-b")
		os.WriteFile(a, []byte("x"), 0o600)
		if src, err := os.Open(a); err == nil {
			if dst, err := os.Create(b); err == nil {
				reflink = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
				dst.Close()
			}
			src.Close()
		}
		os.Remove(a)
		os.Remove(b)
	}
	add("XFS with reflink", reflink, w.Root, fmt.Sprintf("put %s on XFS made with mkfs.xfs -m reflink=1", w.Root))

	err = pressureEvents()
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	add("memory pressure events", err == nil, detail, "boot with psi=1 (or a kernel with CONFIG_PSI), so overcommit reacts at once rather than at the next check")

	classes := w.Classes()
	add("Firecracker", len(classes) > 0, strings.Join(classes, ", "), "install a Firecracker release under "+filepath.Join(w.Root, "firecracker"))
	return out
}
