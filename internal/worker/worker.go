//go:build linux

package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/mouizahmed/hutch/internal/firecracker"
)

// Worker manages boxes under Root (an XFS volume with reflink):
//
//	firecracker/<version>/{firecracker,jailer}
//	kernel/vmlinux                                          Firecracker guest kernel
//	images/<name>/{rootfs.ext4,image.json}                 built by package image
//	boxes/<id>/{box.json,rootfs.ext4,base.mem,console.log,console.in,firecracker.log}
//	boxes/<id>/snapshots/<gen>/{rootfs.ext4,vmstate,mem}   latest snapshot (disk reflinked)
//	boxes/<id>/ref/{rootfs.ext4,vmstate,mem,manifest.json}  what the bucket last stored
//	cache/chunks/                                          downloaded chunks, LRU
//	jail/firecracker/<id>/root/   jailer chroot while the box runs
type Worker struct {
	Root      string
	FCVersion string
	// GVisor is the gVisor version boxes run in on a machine without /dev/kvm; it is
	// empty where boxes are Firecracker VMs. The backend follows the host.
	GVisor string
	// Keys serves each running box's HTTPS_PROXY in its network; nil serves none.
	Keys interface {
		Open(box, ns string, allow []string) error
		Close(ns string)
	}
	vendor string
}

type State string

const (
	Running State = "running"
	Asleep  State = "asleep"
	// Archived boxes keep only their disk; waking them is a cold boot.
	Archived State = "archived"
	// Failed boxes did not boot; they are kept so their console can be read.
	Failed State = "failed"
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
	Idle    Idle   `json:"idle"`
	// Allow lists private targets (host:port) the box may reach despite the blocked ranges.
	Allow []string `json:"allow,omitempty"`
	// Store says where the box's snapshots go in the bucket; nil for boxes never stored.
	Store *Store `json:"store,omitempty"`
	// GVisor is the gVisor version a gVisor box runs (FCVersion is empty then).
	GVisor string `json:"gvisor,omitempty"`
	// RunscID is the internal OCI ID. New sandboxes use fixed-length uppercase
	// IDs, so runsc's abbreviation lookup cannot confuse human box-name prefixes.
	// Empty identifies older running sandboxes that still use the box name.
	RunscID string `json:"runsc_id,omitempty"`
	// Token is what hutchd presents to a gVisor box's hutch-guest. It lives in the
	// box's memory, so it goes wherever the box goes: forks, the bucket, a new machine.
	Token string `json:"token,omitempty"`
	// Uploaded is the generation the bucket last stored.
	Uploaded int `json:"uploaded,omitempty"`
	// Checkpointed is when the box's state was last captured: a snapshot, or the start
	// from one.
	Checkpointed time.Time `json:"checkpointed,omitzero"`
}

// Store places a box's snapshots in the bucket: manifests <Prefix>/<generation>,
// encrypted with Project's key.
type Store struct {
	Project string `json:"project"`
	Prefix  string `json:"prefix"`
}

// Idle is a box's idle policy; zero values mean the defaults.
type Idle struct {
	Timeout      time.Duration `json:"timeout,omitempty"`
	CPUThreshold float64       `json:"cpu_threshold,omitempty"`
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func New(root, fcVersion string) (*Worker, error) {
	vendor, err := firecracker.CPUVendor()
	if err != nil {
		return nil, err
	}
	w := &Worker{Root: root, FCVersion: fcVersion, vendor: vendor}
	if _, err := os.Stat("/dev/kvm"); err == nil {
		for _, f := range []string{"firecracker", "jailer"} {
			if _, err := os.Stat(w.fcBin("", f)); err != nil {
				return nil, fmt.Errorf("firecracker %s not installed: %w", fcVersion, err)
			}
		}
	} else {
		v := w.gvisorVersions()
		if len(v) == 0 {
			return nil, errors.New("no /dev/kvm, and gVisor isn't installed: run hutch setup again")
		}
		w.FCVersion, w.GVisor = "", v[len(v)-1]
	}
	if err := hostNAT(); err != nil {
		return nil, fmt.Errorf("host NAT: %w", err)
	}
	return w, os.MkdirAll(filepath.Join(root, "boxes"), 0o755)
}

// Class is the snapshot class new boxes get: the newest Firecracker's or gVisor's.
func (w *Worker) Class() string {
	if w.GVisor != "" {
		return gvisorClass(w.vendor, w.GVisor)
	}
	return firecracker.SnapshotClass(w.vendor, w.FCVersion)
}

// Classes lists every snapshot class the worker restores warm: one per installed
// Firecracker or gVisor version, kept side by side so old snapshots outlive upgrades.
func (w *Worker) Classes() []string {
	var out []string
	if w.GVisor != "" {
		for _, v := range w.gvisorVersions() {
			out = append(out, gvisorClass(w.vendor, v))
		}
		return out
	}
	for _, v := range w.versions() {
		out = append(out, firecracker.SnapshotClass(w.vendor, v))
	}
	return out
}

// versions lists the installed Firecracker versions.
func (w *Worker) versions() []string {
	entries, _ := os.ReadDir(filepath.Join(w.Root, "firecracker"))
	var out []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(w.Root, "firecracker", e.Name(), "firecracker")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out
}

// warm reports whether the worker can restore a snapshot of class c.
func (w *Worker) warm(c string) bool { return slices.Contains(w.Classes(), c) }

// fcBin is a Firecracker binary of a version ("" for the newest).
func (w *Worker) fcBin(version, name string) string {
	if version == "" {
		version = w.FCVersion
	}
	return filepath.Join(w.Root, "firecracker", version, name)
}
func (w *Worker) imageDir(name string) string { return filepath.Join(w.Root, "images", name) }
func (w *Worker) kernelPath() string          { return filepath.Join(w.Root, "kernel", "vmlinux") }
func (w *Worker) boxDir(id string) string     { return filepath.Join(w.Root, "boxes", id) }
func (w *Worker) jailRoot(id string) string {
	return filepath.Join(w.Root, "jail", "firecracker", id, "root")
}
func (w *Worker) genDir(id string, gen int) string {
	return filepath.Join(w.boxDir(id), "snapshots", fmt.Sprint(gen))
}

func (w *Worker) ConsolePath(id string) string   { return filepath.Join(w.boxDir(id), "console.log") }
func (w *Worker) FCLogPath(id string) string     { return filepath.Join(w.boxDir(id), "firecracker.log") }
func (w *Worker) consoleInPath(id string) string { return filepath.Join(w.boxDir(id), "console.in") }

func (w *Worker) Get(id string) (*Box, error) { return w.load(id) }

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
func (w *Worker) newNetwork(id string, allow []string) (slot, error) {
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
			if err := slot(s).setup(slot(s).uid(), allow, w.GVisor != ""); err != nil {
				return 0, err
			}
			if w.Keys != nil {
				if err := w.Keys.Open(id, slot(s).netnsPath(), allow); err != nil {
					slot(s).teardown()
					return 0, err
				}
			}
			return slot(s), nil
		}
	}
	return 0, errors.New("no free network slots")
}

// teardown removes a slot's network and the box's proxy in it.
func (w *Worker) teardown(s slot) {
	if w.Keys != nil {
		w.Keys.Close(s.netnsPath())
	}
	s.teardown()
}

// OpenKeys serves the proxy of every box running now, as after hutchd restarts.
func (w *Worker) OpenKeys() {
	boxes, err := w.List()
	if err != nil || w.Keys == nil {
		return
	}
	for _, b := range boxes {
		if b.State == Running && b.Slot != 0 && w.Alive(b) {
			if err := w.Keys.Open(b.ID, slot(b.Slot).netnsPath(), b.Allow); err != nil {
				log.Print(err)
			}
		}
	}
}

// uid is the unprivileged user a box's Firecracker runs as.
func (s slot) uid() int { return 100000 + int(s) }
