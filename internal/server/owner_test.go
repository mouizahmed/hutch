package server

import (
	"context"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/durable"
)

// Two copies of one box, the old owner cut off and still checkpointing, never
// write the same manifest, and restores take the new owner's.
func TestOwnersNeverCollide(t *testing.T) {
	ctx := context.Background()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mem := bucket.NewMem()
	s.vault.Store(&vault{bucket: mem})
	st := &durable.Store{Bucket: mem}
	store := s.boxStore("p1", "web", "")
	k := s.master.ProjectKey("p1")
	put := func(owner string, gen int, image string) {
		t.Helper()
		m := &durable.Manifest{Created: time.Now(), Box: &durable.BoxInfo{Image: image}, Files: map[string]*durable.File{}}
		if err := st.PutManifest(ctx, k, durable.ManifestName(store.Prefix, owner, gen), m); err != nil {
			t.Fatal(err)
		}
	}
	latest := func() (string, int) {
		t.Helper()
		owner, gen, err := s.latestSnapshot(ctx, store)
		if err != nil {
			t.Fatal(err)
		}
		return owner, gen
	}

	// A box from before ownership numbers: its names read as owner 0.
	put("", 5, "old")
	if owner, gen := latest(); owner != "" || gen != 5 {
		t.Fatalf("v0.1 names: got %q %d", owner, gen)
	}

	claimed, err := s.claim(ctx, store, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Owner == "" {
		t.Fatal("claim kept owner 0")
	}
	if owner, gen := latest(); owner != claimed.Owner || gen != 5 {
		t.Fatalf("after the claim: got %q %d, want %q 5", owner, gen, claimed.Owner)
	}

	// The cut-off copy writes generations past the new owner's.
	put("", 6, "stale")
	put("", 7, "stale")
	put(claimed.Owner, 6, "new")
	if owner, gen := latest(); owner != claimed.Owner || gen != 6 {
		t.Fatalf("with both owners writing: got %q %d, want %q 6", owner, gen, claimed.Owner)
	}
	m, err := st.GetManifest(ctx, k, durable.ManifestName(store.Prefix, claimed.Owner, 6))
	if err != nil || m.Box.Image != "new" {
		t.Fatalf("the new owner's generation 6: %v %+v", err, m)
	}

	// A later term's numbers beat every number of an earlier one.
	again, err := s.claim(ctx, claimed, claimed.Owner, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !durable.OwnerLess(claimed.Owner, again.Owner) {
		t.Fatalf("second claim %q isn't above %q", again.Owner, claimed.Owner)
	}
	next, err := s.store.nextOwner(12)
	if err != nil {
		t.Fatal(err)
	}
	if !durable.OwnerLess(again.Owner, next) || !durable.OwnerLess("9.40", "10.1") {
		t.Fatalf("terms don't order first: %q, %q", again.Owner, next)
	}

	// Retention keeps the newest by owner, then generation.
	if _, err := pruneBoxSnapshots(ctx, mem, 2); err != nil {
		t.Fatal(err)
	}
	objs, _ := mem.List(ctx, store.Prefix+"/")
	var kept []string
	for _, o := range objs {
		kept = append(kept, o.Key[len(store.Prefix)+1:])
	}
	want := map[string]bool{durable.ManifestName("", again.Owner, 6)[1:]: true, durable.ManifestName("", claimed.Owner, 6)[1:]: true}
	if len(kept) != 2 || !want[kept[0]] || !want[kept[1]] {
		t.Fatalf("pruning kept %v", kept)
	}
}

func TestLoginChangeBacksUp(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.putLayerPart("config", -1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.backupNow:
		t.Fatal("a config change asked for a backup at once")
	default:
	}
	if _, _, err := s.store.putLayerPart(credentialsPrefix+"claude", -1, []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.backupNow:
	default:
		t.Fatal("a stored login didn't ask for a backup")
	}
	if err := s.store.putLayerParts(map[string][]byte{keysPrefix + "codex": []byte("x")}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.backupNow:
	default:
		t.Fatal("a stored refresh didn't ask for a backup")
	}
}
