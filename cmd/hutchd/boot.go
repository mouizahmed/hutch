//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/agent"
	"github.com/mouizahmed/hutch/internal/worker"
)

// Where the worker image keeps what a new worker's data disk starts with.
const imageDir = "/opt/hutch"

// Join is the user data an elastic worker boots with: all it needs to enroll.
type Join struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	Name   string `json:"name"`
}

// boot starts an elastic worker: the first time, it formats and mounts the data disk,
// copies Firecracker and the base image onto it from the worker image, and enrolls
// with the token in the instance's user data. Then it runs.
func boot(ctx context.Context, root, fcVersion string) error {
	if err := mountData(root); err != nil {
		return fmt.Errorf("data disk: %w", err)
	}
	for _, d := range []string{"firecracker", "images"} {
		if _, err := os.Stat(filepath.Join(root, d)); errors.Is(err, os.ErrNotExist) {
			if out, err := exec.Command("cp", "-a", "--sparse=always", filepath.Join(imageDir, d), root).CombinedOutput(); err != nil {
				return fmt.Errorf("copy %s: %v: %s", d, err, out)
			}
		}
	}
	id := agent.Identity{Dir: filepath.Join(root, "identity")}
	nameFile := filepath.Join(root, "name")
	if _, err := os.Stat(filepath.Join(id.Dir, "cert.pem")); errors.Is(err, os.ErrNotExist) {
		data, err := userData(ctx)
		if err != nil {
			return fmt.Errorf("user data: %w", err)
		}
		var j Join
		if err := json.Unmarshal(data, &j); err != nil || j.Server == "" || j.Token == "" {
			return fmt.Errorf("user data is not a Hutch join: %v", err)
		}
		if _, err := agent.Enroll(ctx, id, j.Server, j.Token, j.Name); err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		if err := os.WriteFile(nameFile, []byte(j.Name), 0o644); err != nil {
			return err
		}
	}
	name, err := os.ReadFile(nameFile)
	if err != nil {
		return err
	}
	w, err := worker.New(root, fcVersion)
	if err != nil {
		return err
	}
	return agent.Run(ctx, id, w, strings.TrimSpace(string(name)))
}

// mountData mounts the volume labelled hutch at root, formatting the instance's
// blank disk as XFS with reflink first if there is none.
func mountData(root string) error {
	if out, _ := exec.Command("findmnt", "-n", root).Output(); len(out) > 0 {
		return nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if out, _ := exec.Command("blkid", "-L", "hutch").Output(); len(out) == 0 {
		dev, err := blankDisk()
		if err != nil {
			return err
		}
		if out, err := exec.Command("mkfs.xfs", "-q", "-L", "hutch", "-m", "reflink=1", dev).CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs.xfs %s: %v: %s", dev, err, out)
		}
	}
	if out, err := exec.Command("mount", "LABEL=hutch", root).CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %v: %s", err, out)
	}
	return nil
}

// blankDisk finds the disk with no partitions and no filesystem.
func blankDisk() (string, error) {
	out, err := exec.Command("lsblk", "-nrpo", "NAME,TYPE,FSTYPE,PKNAME").Output()
	if err != nil {
		return "", err
	}
	type dev struct{ name, typ, fs, parent string }
	var devs []dev
	parents := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, " ")
		for len(f) < 4 {
			f = append(f, "")
		}
		devs = append(devs, dev{f[0], f[1], f[2], f[3]})
		if f[3] != "" {
			parents[f[3]] = true
		}
	}
	for _, d := range devs {
		if d.typ == "disk" && d.fs == "" && !parents[d.name] {
			return d.name, nil
		}
	}
	return "", errors.New("no blank disk for worker data")
}

// userData reads the instance's user data through IMDSv2.
func userData(ctx context.Context) ([]byte, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	token, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/user-data", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err = c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IMDS: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}
