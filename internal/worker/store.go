//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
)

// The files of a snapshot, a Firecracker VM's or a gVisor sandbox's; an archived
// box has only its disk.
var snapshotFiles = append([]string{"rootfs.ext4", "mem", "vmstate"}, checkpointFiles...)

func (w *Worker) refDir(id string) string { return filepath.Join(w.boxDir(id), "ref") }

// ManifestName is where generation gen of a box with store s is in the bucket.
func ManifestName(s *Store, gen int) string { return fmt.Sprintf("%s/%010d", s.Prefix, gen) }

// cloneDir reflinks every regular file of src into a new dst.
func cloneDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if err := clone(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Pending reports whether the box has a generation the bucket doesn't have yet.
func (b *Box) Pending() bool { return b.Store != nil && b.Uploaded < b.Generation }

// Upload stores the box's latest generation in the bucket. Only chunks whose blocks
// changed since the last upload are read; the rest are taken from its manifest.
func (w *Worker) Upload(ctx context.Context, id string, s *durable.Store, k durable.Key) (map[string]durable.Stats, error) {
	st := map[string]durable.Stats{}
	work := filepath.Join(w.boxDir(id), "upload")
	b, err := w.prepareUpload(id, work)
	if err != nil || b == nil {
		return st, err
	}
	defer os.RemoveAll(work)

	var ref *durable.Manifest
	if data, err := os.ReadFile(filepath.Join(w.refDir(id), "manifest.json")); err == nil {
		ref = &durable.Manifest{}
		if json.Unmarshal(data, ref) != nil {
			ref = nil
		}
	}
	m := &durable.Manifest{Created: time.Now().UTC(), Files: map[string]*durable.File{},
		Box: &durable.BoxInfo{Image: b.Image, VCPUs: b.VCPUs, MemMiB: b.MemMiB, FCVersion: b.FCVersion, GVisor: b.GVisor, Class: b.Class,
			IdleTimeout: b.Idle.Timeout, CPUThreshold: b.Idle.CPUThreshold, Allow: b.Allow, Watch: b.Watch}}
	for _, name := range snapshotFiles {
		f, err := os.Open(filepath.Join(work, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return st, err
		}
		var refFile *durable.File
		if ref != nil {
			refFile = ref.Files[name]
		}
		file, fst, err := uploadFile(ctx, s, k, f, filepath.Join(w.refDir(id), name), refFile)
		f.Close()
		if err != nil {
			return st, fmt.Errorf("upload %s %s: %w", id, name, err)
		}
		m.Files[name] = file
		st[name] = fst
	}
	m.Box.Archived = m.Files["mem"] == nil && m.Files["pages.img"] == nil
	name := ManifestName(b.Store, b.Generation)
	// Already there: an earlier attempt stored it before failing to record that.
	if err := s.PutManifest(ctx, k, name, m); err != nil && !errors.Is(err, bucket.ErrExists) {
		return st, err
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), data, 0o600); err != nil {
		return st, err
	}
	return st, w.finishUpload(id, work, b)
}

// prepareUpload reflinks the latest generation into work under the box's lock, so
// the upload reads files nothing changes. It returns nil when there's nothing to do.
func (w *Worker) prepareUpload(id, work string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil || !b.Pending() {
		return nil, err
	}
	os.RemoveAll(work)
	if err := cloneDir(w.genDir(id, b.Generation), work); err != nil {
		os.RemoveAll(work)
		return nil, err
	}
	return b, nil
}

// finishUpload makes the uploaded files the new reference, unless the box was
// removed or moved meanwhile.
func (w *Worker) finishUpload(id, work string, uploaded *Box) error {
	unlock, err := w.lock(id)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return err
	}
	if b.Store == nil || *b.Store != *uploaded.Store {
		return nil
	}
	ref := w.refDir(id)
	os.RemoveAll(ref)
	if err := os.Rename(work, ref); err != nil {
		return err
	}
	b.Uploaded = uploaded.Generation
	return w.save(b)
}

// uploadFile stores f, reusing chunks of the reference copy at refPath (whose
// manifest entry is refFile) where its blocks are unchanged.
func uploadFile(ctx context.Context, s *durable.Store, k durable.Key, f *os.File, refPath string, refFile *durable.File) (*durable.File, durable.Stats, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, durable.Stats{}, err
	}
	var refData *os.File
	if refFile != nil {
		if rf, err := os.Open(refPath); err == nil {
			defer rf.Close()
			refData = rf
		} else {
			refFile = nil
		}
	}
	change, err := changes(f, refData)
	if err != nil {
		return nil, durable.Stats{}, err
	}
	return s.PutFile(ctx, k, f, fi.Size(), refFile, change)
}

// Restore creates a box from a manifest in the bucket. It is asleep, ready to wake
// warm, or archived (to cold boot) when the manifest has no memory or was taken on
// another snapshot class.
func (w *Worker) Restore(ctx context.Context, id string, store *Store, gen int, s *durable.Store, k durable.Key) (*Box, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid box id %q", id)
	}
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := os.Stat(w.boxDir(id)); err == nil {
		return nil, fmt.Errorf("box %q already exists", id)
	}
	name := ManifestName(store, gen)
	m, err := s.GetManifest(ctx, k, name)
	if err != nil {
		return nil, err
	}
	if m.Box == nil || m.Files["rootfs.ext4"] == nil {
		return nil, fmt.Errorf("%s is not a box snapshot", name)
	}
	warm := !m.Box.Archived && w.warm(m.Box.Class)
	tmp := filepath.Join(w.Root, "boxes", ".restore-"+id)
	os.RemoveAll(tmp)
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	work := filepath.Join(tmp, "ref")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	for _, f := range snapshotFiles {
		if m.Files[f] == nil || (!warm && f != "rootfs.ext4") {
			continue
		}
		out, err := os.OpenFile(filepath.Join(work, f), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		err = s.GetFile(ctx, k, m.Files[f], out)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("restore %s: %w", f, err)
		}
	}
	if s.Cache != nil {
		s.Cache.Evict()
	}
	genDir := filepath.Join(tmp, "snapshots", fmt.Sprint(gen))
	if err := cloneDir(work, genDir); err != nil {
		return nil, err
	}
	if err := clone(filepath.Join(work, "rootfs.ext4"), filepath.Join(tmp, "rootfs.ext4")); err != nil {
		return nil, err
	}
	stored := m
	if !warm {
		// The reference must match the files kept: the disk only.
		stored = &durable.Manifest{Format: m.Format, Created: m.Created, Box: m.Box, Files: map[string]*durable.File{"rootfs.ext4": m.Files["rootfs.ext4"]}}
	}
	data, _ := json.Marshal(stored)
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, w.boxDir(id)); err != nil {
		return nil, err
	}
	ok = true
	b := &Box{ID: id, Image: m.Box.Image, VCPUs: m.Box.VCPUs, MemMiB: m.Box.MemMiB, FCVersion: m.Box.FCVersion, GVisor: m.Box.GVisor, Class: m.Box.Class,
		State: Asleep, Generation: gen, Uploaded: gen, Created: time.Now().UTC(), Store: store,
		Idle: Idle{Timeout: m.Box.IdleTimeout, CPUThreshold: m.Box.CPUThreshold}, Allow: m.Box.Allow, Watch: m.Box.Watch}
	if !warm {
		b.State = Archived
	}
	if err := w.save(b); err != nil {
		os.RemoveAll(w.boxDir(id))
		return nil, err
	}
	return b, nil
}

// Archive drops a sleeping box's memory, keeping its disk: a new generation holding
// only the disk, which the next upload stores so the bucket can drop the memory too.
func (w *Worker) Archive(id string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Asleep {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	gen := w.genDir(id, b.Generation+1)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return nil, err
	}
	if err := clone(filepath.Join(w.boxDir(id), "rootfs.ext4"), filepath.Join(gen, "rootfs.ext4")); err != nil {
		os.RemoveAll(gen)
		return nil, err
	}
	prev := b.Generation
	b.State, b.Generation = Archived, prev+1
	if err := w.save(b); err != nil {
		return nil, err
	}
	os.RemoveAll(w.genDir(id, prev))
	return b, nil
}

// coldWake boots an archived box from its disk. Its processes start fresh.
func (w *Worker) coldWake(ctx context.Context, b *Box) (*Box, error) {
	err := w.coldStart(ctx, b)
	if err != nil {
		b.State, b.PID = Archived, 0
		w.save(b)
		return nil, err
	}
	return b, nil
}

// Resize changes a sleeping box's size. Its memory snapshot no longer fits the VM,
// so it is dropped as in Archive and the next wake is a cold boot. Disks only grow.
func (w *Worker) Resize(id string, vcpus, memMiB int, diskBytes int64) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Asleep && b.State != Archived {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	rootfs := filepath.Join(w.boxDir(id), "rootfs.ext4")
	if diskBytes > 0 {
		fi, err := os.Stat(rootfs)
		if err != nil {
			return nil, err
		}
		if diskBytes < fi.Size() {
			return nil, fmt.Errorf("disks only grow: %s is %d GB", id, fi.Size()>>30)
		}
		if err := growDisk(rootfs, diskBytes); err != nil {
			return nil, err
		}
	}
	gen := w.genDir(id, b.Generation+1)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		return nil, err
	}
	if err := clone(rootfs, filepath.Join(gen, "rootfs.ext4")); err != nil {
		os.RemoveAll(gen)
		return nil, err
	}
	prev := b.Generation
	if vcpus > 0 {
		b.VCPUs = vcpus
	}
	if memMiB > 0 {
		b.MemMiB = memMiB
	}
	b.State, b.Generation = Archived, prev+1
	if err := w.save(b); err != nil {
		return nil, err
	}
	os.RemoveAll(w.genDir(id, prev))
	return b, nil
}

// RollBack resets a box whose VM died (killed when the worker ran out of memory)
// to its newest snapshot: memory and the disk copy taken with it, so they match.
// It is then asleep, or archived if that snapshot kept only the disk. A box never
// snapshotted can't be rolled back and is marked failed.
func (w *Worker) RollBack(id string) (*Box, error) {
	unlock, err := w.lock(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := w.load(id)
	if err != nil {
		return nil, err
	}
	if b.State != Running || w.Alive(b) {
		return nil, fmt.Errorf("box %q is %s", id, b.State)
	}
	w.stop(b)
	if b.Slot != 0 {
		slot(b.Slot).teardown()
	}
	b.Slot, b.BaseMem = 0, ""
	gen := w.genDir(id, b.Generation)
	disk := filepath.Join(gen, "rootfs.ext4")
	if _, err := os.Stat(disk); b.Generation == 0 || err != nil {
		b.State = Failed
		return b, w.save(b)
	}
	rootfs := filepath.Join(w.boxDir(id), "rootfs.ext4")
	os.Remove(rootfs + ".rollback")
	if err := clone(disk, rootfs+".rollback"); err != nil {
		return nil, err
	}
	if err := os.Rename(rootfs+".rollback", rootfs); err != nil {
		return nil, err
	}
	b.State = Archived
	for _, mem := range []string{"mem", "pages.img"} {
		if _, err := os.Stat(filepath.Join(gen, mem)); err == nil {
			b.State = Asleep
		}
	}
	return b, w.save(b)
}
