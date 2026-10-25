//go:build linux

package guest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

const boxIDFile = "/var/lib/hutch-guest/box-id"

func restore(r guestproto.Restore) error {
	if err := setClock(r.UnixNano); err != nil {
		return fmt.Errorf("set clock: %w", err)
	}
	if len(r.Seed) > 0 {
		if err := os.WriteFile("/dev/urandom", r.Seed, 0); err != nil {
			return fmt.Errorf("seed RNG: %w", err)
		}
	}
	if r.Hostname != "" {
		if err := setHostname(r.Hostname); err != nil {
			return fmt.Errorf("set hostname: %w", err)
		}
	}

	prev, _ := os.ReadFile(boxIDFile)
	if r.BoxID == "" || strings.TrimSpace(string(prev)) == r.BoxID {
		return nil
	}
	if err := newMachineID(); err != nil {
		return fmt.Errorf("machine-id: %w", err)
	}
	if err := newSSHHostKeys(); err != nil {
		return fmt.Errorf("ssh host keys: %w", err)
	}
	if err := os.MkdirAll("/var/lib/hutch-guest", 0o755); err != nil {
		return err
	}
	return os.WriteFile(boxIDFile, []byte(r.BoxID+"\n"), 0o644)
}

// setClock only moves the clock forward: a restored guest is always behind.
func setClock(unixNano int64) error {
	if unixNano <= time.Now().UnixNano() {
		return nil
	}
	tv := syscall.NsecToTimeval(unixNano)
	return syscall.Settimeofday(&tv)
}

func setHostname(name string) error {
	if err := syscall.Sethostname([]byte(name)); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/hostname", []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	hosts := fmt.Sprintf("127.0.0.1 localhost\n127.0.1.1 %s\n", name)
	return os.WriteFile("/etc/hosts", []byte(hosts), 0o644)
}

func newMachineID() error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return os.WriteFile("/etc/machine-id", []byte(hex.EncodeToString(b)+"\n"), 0o444)
}

func newSSHHostKeys() error {
	old, _ := filepath.Glob("/etc/ssh/ssh_host_*")
	for _, f := range old {
		os.Remove(f)
	}
	for _, t := range []string{"ed25519", "ecdsa"} {
		key := "/etc/ssh/ssh_host_" + t + "_key"
		if out, err := exec.Command("ssh-keygen", "-q", "-t", t, "-N", "", "-f", key).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	// A running sshd reloads its host keys on SIGHUP.
	if b, err := os.ReadFile("/run/sshd.pid"); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			syscall.Kill(pid, syscall.SIGHUP)
		}
	}
	return nil
}
