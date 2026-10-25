package firecracker

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Firecracker v1.17.0's T2A (AMD) and T2CL (Intel) templates. T2CL still exposes
// AMX on Granite Rapids; Hutch's own Intel template must mask it (risks.md).
//
//go:embed templates/*.json
var templates embed.FS

const TemplateName = "t2"

// CPUVendor returns "amd" or "intel" for this host.
func CPUVendor() (string, error) {
	if runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), ":")
		if !ok || strings.TrimSpace(k) != "vendor_id" {
			continue
		}
		switch strings.TrimSpace(v) {
		case "AuthenticAMD":
			return "amd", nil
		case "GenuineIntel":
			return "intel", nil
		default:
			return "", fmt.Errorf("unsupported CPU vendor %q", strings.TrimSpace(v))
		}
	}
	return "", fmt.Errorf("no vendor_id in /proc/cpuinfo")
}

func CPUTemplate(vendor string) (json.RawMessage, error) {
	return templates.ReadFile("templates/" + vendor + ".json")
}

// SnapshotClass names the set of hosts that can restore each other's memory snapshots.
func SnapshotClass(vendor, fcVersion string) string {
	return fmt.Sprintf("x86_64-%s-%s/fc-%s", vendor, TemplateName, strings.TrimPrefix(fcVersion, "v"))
}
