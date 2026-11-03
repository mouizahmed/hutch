package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
)

// Each machine is placed by its own machine config, in placement.order, and a
// box asked for on a full machine waits for it rather than going elsewhere.
func TestPlaceAcrossMachines(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A removed machine named like a current one.
	if err := s.store.ensureWorker("w-gone", "third"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.setWorkerGone("w-gone"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id, name string
		memMiB   int
	}{{"w-big", "big", 20480}, {"w-small", "small", 16384}, {"w-third", "third", 16384}} {
		info := api.Worker{Name: m.name, Class: "intel", CPUs: 8, MemMiB: m.memMiB}
		if err := s.store.ensureWorker(m.id, m.name); err != nil {
			t.Fatal(err)
		}
		if err := s.store.workerSeen(m.id, info); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.workers[m.id] = &workerConn{id: m.id, info: info, projects: map[string]bool{}}
		s.mu.Unlock()
	}
	// third has a box awake, so it's the fullest that fits.
	if err := s.store.putBox(api.Box{ID: "busy", Worker: "w-third", State: "running", VCPUs: 2, MemMiB: 4096, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	set := func(on, content string) {
		t.Helper()
		name := "machine"
		if on != "" {
			name = machineDoc(on)
		}
		d, err := s.store.config(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.store.putConfig(name, d.Version, content); err != nil {
			t.Fatal(err)
		}
	}
	box := need{memMiB: 4096, vcpus: 2, class: "intel"}
	placed := func(requested string) (string, error) {
		t.Helper()
		w, release, err := s.place(box, requested)
		if err != nil {
			return "", err
		}
		release()
		return w.info.Name, nil
	}
	expect := func(want string) {
		t.Helper()
		if got, err := placed(""); err != nil || got != want {
			t.Fatalf("placed on %q (%v), want %s", got, err, want)
		}
	}

	expect("third")
	set("", "placement.order = \"spread\"\n")
	expect("big")
	// big's own margin leaves it less free than small.
	set("w-big", "machine.memory_margin = \"30%\"\n")
	expect("small")
	set("", "")
	// third's own cap takes it out, leaving big the fullest.
	set("w-third", "machine.max_boxes = 1\n")
	expect("big")

	_, err = placed("third")
	if !errors.Is(err, errNoRoom) || !strings.Contains(err.Error(), "machine.max_boxes (1) reached") {
		t.Fatalf("on a full machine by name: %v, want no room there (so it queues)", err)
	}
	third := s.workers["w-third"]
	s.mu.Lock()
	delete(s.workers, "w-third")
	s.mu.Unlock()
	if _, err := placed("third"); err == nil || errors.Is(err, errNoRoom) || !strings.Contains(err.Error(), "machine third is offline") {
		t.Fatalf("on an offline machine: %v", err)
	}

	// A box moving asleep needs a machine big enough, not room among awake boxes:
	// third is at its cap and big is too full of awake boxes for it awake.
	if err := s.store.putBox(api.Box{ID: "hog", Worker: "w-big", State: "running", VCPUs: 2, MemMiB: 12288, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.workers["w-third"] = third
	s.mu.Unlock()
	big := need{memMiB: 8192, vcpus: 2, class: "intel", avoid: "w-small"}
	if _, _, err := s.place(big, ""); !errors.Is(err, errNoRoom) {
		t.Fatalf("awake, an 8 GB box placed: %v", err)
	}
	big.asleep = true
	w, release, err := s.place(big, "third")
	if err != nil {
		t.Fatalf("asleep, on a machine at max_boxes: %v", err)
	}
	if s.placing["w-third"] != 0 || s.starting != 0 {
		t.Fatal("a box moving asleep counted as starting")
	}
	release()
	big.memMiB = 20480
	if _, _, err := s.place(big, ""); !errors.Is(err, errNoRoom) {
		t.Fatalf("asleep, a box bigger than every machine placed on %v", w.info.Name)
	}

	// A machine being removed takes nothing, even asked for by name.
	if err := s.store.setDraining("w-big", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.place(need{memMiB: 1024, vcpus: 1, class: "intel"}, "big"); err == nil || !strings.Contains(err.Error(), "being removed") {
		t.Fatalf("on a machine being removed: %v", err)
	}
	if err := s.store.setDraining("w-big", false); err != nil {
		t.Fatal(err)
	}

	// A wake already admitted against limits.awake_boxes isn't counted again.
	set("", "limits.awake_boxes = 3\n")
	s.starting = 1 // the wake's own admission
	if _, _, err := s.place(need{memMiB: 1024, vcpus: 1, class: "intel"}, ""); err == nil {
		t.Fatal("placed past limits.awake_boxes")
	}
	w, release, err = s.place(need{memMiB: 1024, vcpus: 1, class: "intel", admitted: true}, "")
	if err != nil || s.starting != 1 {
		t.Fatalf("an admitted wake: %v, starting %d", err, s.starting)
	}
	release()
	s.starting = 0
	set("", "")

	// A machine that coordinates only takes nothing, even asked for by name.
	set("w-small", "machine.run_boxes = false\n")
	if _, _, err := s.place(need{memMiB: 1024, vcpus: 1, class: "intel"}, "small"); err == nil || !strings.Contains(err.Error(), "doesn't run boxes") {
		t.Fatalf("on a machine that doesn't run boxes: %v", err)
	}
	if w, release, err := s.place(need{memMiB: 1024, vcpus: 1, class: "intel"}, ""); err != nil || w.id == "w-small" {
		t.Fatalf("placed on %v (%v) with small not running boxes", w, err)
	} else {
		release()
	}

	p, _ := json.Marshal(api.ConfigPutParams{Name: "machine", On: "small", BaseVersion: 1, Content: "machine.memory_margin = \"40%\"\nplacement.order = \"spread\"\n"})
	if _, err := s.configPut(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "placement.order is the same on every machine") {
		t.Fatalf("a fleet-wide key set on one machine: %v", err)
	}
}

// Settings are parsed once per change, not on every placement, and a change is
// seen at once.
func TestSettingsCached(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.settingsOn("w-a")
	if b, _ := s.settingsOn("w-a"); a != b {
		t.Fatal("settings parsed again with nothing changed")
	}
	d, _ := s.store.config(machineDoc("w-a"))
	if _, _, err := s.store.putConfig(machineDoc("w-a"), d.Version, "machine.max_boxes = 3\n"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.settingsOn("w-a"); c.Int("machine.max_boxes") != 3 {
		t.Fatal("a config change wasn't seen")
	}
}
