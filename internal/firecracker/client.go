package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

type Client struct {
	http *http.Client
}

func NewClient(socketPath string) *Client {
	// No keep-alive: clients are short-lived, and Firecracker refuses connections
	// beyond a small limit, so idle ones left open would lock hutchd out.
	return &Client{http: &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}}}
}

// WaitForSocket waits until Firecracker's API socket accepts connections.
func WaitForSocket(ctx context.Context, socketPath string) error {
	for {
		if _, err := os.Stat(socketPath); err == nil {
			if c, err := net.Dial("unix", socketPath); err == nil {
				c.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker API socket %s: %w", socketPath, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

type MachineConfig struct {
	VCPUs           int  `json:"vcpu_count"`
	MemMiB          int  `json:"mem_size_mib"`
	SMT             bool `json:"smt"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type Drive struct {
	ID           string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	IOEngine     string `json:"io_engine,omitempty"`
}

type NetworkInterface struct {
	ID          string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type Balloon struct {
	AmountMiB         int  `json:"amount_mib"`
	DeflateOnOOM      bool `json:"deflate_on_oom"`
	StatsPollingSecs  int  `json:"stats_polling_interval_s"`
	FreePageReporting bool `json:"free_page_reporting"`
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}

func (c *Client) PutCPUConfig(ctx context.Context, template json.RawMessage) error {
	return c.do(ctx, http.MethodPut, "/cpu-config", template)
}

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.ID, d)
}

func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+n.ID, n)
}

func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}

func (c *Client) PutBalloon(ctx context.Context, b Balloon) error {
	return c.do(ctx, http.MethodPut, "/balloon", b)
}

func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// CreateDiffSnapshot writes the pages dirtied since boot or the last snapshot.
func (c *Client) CreateDiffSnapshot(ctx context.Context, statePath, memPath string) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", map[string]string{
		"snapshot_type": "Diff",
		"snapshot_path": statePath,
		"mem_file_path": memPath,
	})
}

func (c *Client) LoadSnapshot(ctx context.Context, statePath, memPath string) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path":     statePath,
		"mem_backend":       map[string]string{"backend_type": "File", "backend_path": memPath},
		"track_dirty_pages": true,
		"resume_vm":         true,
	})
}
