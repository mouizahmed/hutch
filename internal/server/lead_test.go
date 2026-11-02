package server

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/link"
)

// leadOn is a lead with workers and bucket b, sharing the link CA of from.
func leadOn(t *testing.T, b bucket.Bucket, from *Server, local string) *Server {
	t.Helper()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.vault.Store(&vault{bucket: b})
	if from == nil {
		if _, err := s.store.ensureLink("lead.example"); err != nil {
			t.Fatal(err)
		}
	} else {
		l, _ := from.store.link()
		keyPEM, _ := from.store.userKey.Open("link/ca-key", mustSealed(t, from))
		if _, err := s.store.exec(`INSERT INTO link (id, ca_cert, ca_key, address, term) VALUES (1, ?, ?, ?, 0)`,
			l.ca.PEM, s.store.userKey.Seal("link/ca-key", keyPEM), local+".example"); err != nil {
			t.Fatal(err)
		}
	}
	s.local = local
	return s
}

func mustSealed(t *testing.T, s *Server) []byte {
	var sealed []byte
	if err := s.store.db.QueryRow(`SELECT ca_key FROM link WHERE id = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	return sealed
}

// One lead writes its term's record; machines restored from its backup claim
// terms above it, never the same one, and the lead whose term is passed stops
// placing boxes, backing up, and collecting garbage.
func TestLeadTerms(t *testing.T) {
	ctx := t.Context()
	b := bucket.NewMem()
	old := leadOn(t, b, nil, "w-old")
	if err := old.checkLead(ctx); err != nil {
		t.Fatal(err)
	}
	l, _ := old.store.link()
	if r, ok, _ := link.HighestRecord(ctx, b, l.ca.Cert); !ok || r.Term != 1 || r.WorkerID != "w-old" {
		t.Fatalf("the first lead's record: %+v %v", r, ok)
	}

	// Two restored machines claim at once.
	a, c := leadOn(t, b, old, "w-a"), leadOn(t, b, old, "w-c")
	var wg sync.WaitGroup
	for _, s := range []*Server{a, c} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.checkLead(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ta, tc := a.store.term(), c.store.term()
	if ta < 2 || tc < 2 || ta == tc {
		t.Fatalf("claimed terms %d and %d: want two distinct terms above 1", ta, tc)
	}
	winner, loser := a, c
	if tc > ta {
		winner, loser = c, a
	}
	if err := winner.checkLead(ctx); err != nil {
		t.Fatalf("the highest term's lead: %v", err)
	}
	// One just started, before its own worker attaches, steps down as well.
	old.local = ""
	fenced := map[*Server]string{}
	for _, s := range []*Server{old, loser} {
		s.cfg.Fence = func(why string) { fenced[s] = why }
	}
	for _, s := range []*Server{old, loser} {
		if err := s.checkLead(ctx); !errors.Is(err, errNotLead) {
			t.Fatalf("a passed lead's check: %v", err)
		}
		if _, _, err := s.place(need{memMiB: 1024, vcpus: 1}, ""); !errors.Is(err, errNotLead) {
			t.Fatalf("a passed lead placed a box: %v", err)
		}
		if err := s.backup(ctx); !errors.Is(err, errNotLead) {
			t.Fatalf("a passed lead backed up: %v", err)
		}
		if _, err := s.gc(ctx); !errors.Is(err, errNotLead) {
			t.Fatalf("a passed lead collected garbage: %v", err)
		}
	}
	if len(fenced) != 2 || !strings.Contains(fenced[old], "leads at term") {
		t.Fatalf("passed leads fenced themselves: %v", fenced)
	}
}

// A restore takes the newest backup of the highest term: a passed lead's later
// backups never win.
func TestBackupOrder(t *testing.T) {
	now := time.Now()
	keys := []string{
		"server/backups/" + "0000000000001700000000000"[:16], // before terms: term 0
		backupName(2, now.Add(-time.Hour)),
		backupName(1, now), // the passed lead, still writing
		backupName(2, now.Add(-2*time.Hour)),
	}
	sort.Slice(keys, func(i, j int) bool { return newerBackup(keys[i], keys[j]) })
	if keys[0] != backupName(2, now.Add(-time.Hour)) || keys[3] != "server/backups/0000000000001700" {
		t.Fatalf("order: %v", keys)
	}
	if term, at, ok := parseBackup("server/backups/0000001700000000"); !ok || term != 0 || at.UnixMilli() != 1700000000 {
		t.Fatalf("a name from before terms: %d %v %v", term, at, ok)
	}
}
