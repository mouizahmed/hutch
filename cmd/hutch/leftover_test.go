package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mouizahmed/hutch/internal/pki"
)

// A machine with boxes from another setup isn't added; one of this setup
// before (a worker trusting this lead's CA, or an old lead keeping it aside) is. The probe names the CA as tokens do.
func TestLeftoverBoxes(t *testing.T) {
	ca, _, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	// The probe's fingerprint of a ca.pem, as the probe script computes it.
	probeCA := func(pem []byte) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, pem, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", `sed '/-----/d' "$1" | base64 -d | sha256sum | cut -c1-64`, "sh", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	// Machines are Linux; there the probe's fingerprint must be the token's.
	if runtime.GOOS == "linux" {
		if got := probeCA(ca.PEM); got != pki.Fingerprint(ca.Cert) {
			t.Fatalf("the probe's fingerprint %s isn't the token's %s", got, pki.Fingerprint(ca.Cert))
		}
	}
	if runtime.GOOS != "linux" {
		probeCA = func(pem []byte) string {
			c, err := pki.ParseCert(pem)
			if err != nil {
				t.Fatal(err)
			}
			return pki.Fingerprint(c)
		}
	}
	token := pki.Fingerprint(ca.Cert) + ".secret"
	check := func(boxes, linkCA string) error {
		return leftoverBoxes(probe{values: map[string]string{"boxes": boxes, "link_ca": linkCA}}, "m", token)
	}
	if err := check("", ""); err != nil {
		t.Fatalf("an empty machine: %v", err)
	}
	if err := check("web-1 api-2 ", " "+probeCA(ca.PEM)); err != nil {
		t.Fatalf("a worker added here before: %v", err)
	}
	// An old lead fenced by this setup keeps its CA beside its state, perhaps
	// with another setup's CA from when it was a worker there.
	if err := check("web-1 api-2 ", " "+probeCA(other.PEM)+" "+probeCA(ca.PEM)); err != nil {
		t.Fatalf("an old lead of this setup: %v", err)
	}
	for _, linkCA := range []string{" " + probeCA(other.PEM), ""} {
		if err := check("web-1 api-2 ", linkCA); err == nil || !strings.Contains(err.Error(), "web-1, api-2") {
			t.Fatalf("boxes from another setup (CAs %q): %v", linkCA, err)
		}
	}
}
