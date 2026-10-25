//go:build linux

package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/firecracker"
)

// Worker manages boxes under Root (an XFS volume with reflink):
//
//	firecracker/<version>/{firecracker,jailer,rebase-snap}
//	images/<name>/{vmlinux,rootfs.ext4}
//	boxes/<id>/{box.json,rootfs.ext4,base.mem,console.log,snapshots/<gen>/{vmstate,mem}}
//	jail/firecracker/<id>/root/   jailer chroot while the box runs
type Worker struct {
	Root      string
	FCVersion string
	vendor    string
}

type State string

const (
	Running State = "running"
	Asleep  State = "asleep"
)

type Box struct {
	ID         string    `json:"id"`
	Image      string    `json:"image"`
	VCPUs      int       `json:"vcpus"`
	MemMiB     int       `json:"mem_mib"`
	State      State     `json:"state"`
	Slot       int       `json:"slot,omitempty"`
	PID        int       `json:"pid,omitempty"`
	Generation int       `json:"generation"`
	FCVersion  string    `json:"fc_version"`
	Class      string    `json:"snapshot_class"`
	Created    time.Time `json:"created"`
	// BaseMem is the memory file the running VM was restored from (empty after a cold
	// boot); the next diff snapshot is applied on top of a copy of it.
	BaseMem string `json:"base_mem,omitempty"`
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func New(root, fcVersion string) (*Worker, error) {
	vendor, err := firecracker.CPUVendor()
	if err != nil {
		return nil, err
	}
	w := &Worker{Root: root, FCVersion: fcVersion, vendor: vendor}
	for _, f := range []string{"firecracker", "jailer", "rebase-snap"} {
		if _, err := os.Stat(w.fcBin(f)); err != nil {
			return nil, fmt.Errorf("firecracker %s not installed: %w", fcVersion, err)
		}
	}
	if err := hostNAT(); err != nil {
		return nil, fmt.Errorf("host NAT: %w", err)
	}
	return w, os.MkdirAll(filepath.Join(root, "boxes"), 0o755)
}

func (w *Worker) Class() string { return firecracker.SnapshotClass(w.vendor, w.FCVersion) }

func (w *Worker) fcBin(name string) string {
	return filepath.Join(w.Root, "firecracker", w.FCVersion, name)
}
func (w *Worker) imageDir(name string) string { return filepath.Join(w.Root, "images", name) }
func (w *Worker) boxDir(id string) string     { return filepath.Join(w.Root, "boxes", id) }
func (w *Worker) jailRoot(id string) string {
	return filepath.Join(w.Root, "jail", "firecracker", id, "root")
}
func (w *Worker) genDir(id string, gen int) string {
	return filepath.Join(w.boxDir(id), "snapshots", fmt.Sprint(gen))
}

func (w *Worker) ConsolePath(id string) string { return filepath.Join(w.boxDir(id), "console.log") }

func (w *Worker) load(id string) (*Box, error) {
	b, err := os.ReadFile(filepath.Join(w.boxDir(id), "box.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no box %q", id)
	}
	if err != nil {
		return nil, err
	}
	var box Box
	return &box, json.Unmarshal(b, &box)
}

func (w *Worker) save(b *Box) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.boxDir(b.ID), "box.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(w.boxDir(b.ID), "box.json"))
}

// lock serializes operations on one box (or on slot allocation, with id "").
func (w *Worker) lock(id string) (func(), error) {
	name := filepath.Join(w.Root, "boxes", ".slots.lock")
	if id != "" {
		name = filepath.Join(w.Root, "boxes", "."+id+".lock")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (w *Worker) List() ([]*Box, error) {
	entries, err := os.ReadDir(filepath.Join(w.Root, "boxes"))
	if err != nil {
		return nil, err
	}
	var boxes []*Box
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := w.load(e.Name())
		if err != nil {
			continue
		}
		boxes = append(boxes, b)
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Created.Before(boxes[j].Created) })
	return boxes, nil
}

// newNetwork picks a free slot and creates its netns under the lock, so concurrent
// starts can't pick the same slot.
func (w *Worker) newNetwork() (slot, error) {
	unlock, err := w.lock("")
	if err != nil {
		return 0, err
	}
	defer unlock()
	boxes, err := w.List()
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, b := range boxes {
		if b.Slot != 0 {
			used[b.Slot] = true
		}
	}
	for s := 1; s <= maxSlot; s++ {
		if !used[s] {
			if _, err := os.Stat(slot(s).netnsPath()); err == nil {
				continue
			}
			return slot(s), slot(s).setup(slot(s).uid())
		}
	}
	return 0, errors.New("no free network slots")
}

// uid is the unprivileged user a box's Firecracker runs as.
func (s slot) uid() int { return 100000 + int(s) }
