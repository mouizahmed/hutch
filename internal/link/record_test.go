package link

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/pki"
)

// Only records signed with the link CA count: one from a machine with just the
// bucket's credentials, however high its term, is never followed.
func TestFollowSignedRecordsOnly(t *testing.T) {
	ctx := t.Context()
	ca, _, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	b := bucket.NewMem()
	d := Dir(t.TempDir())
	for name, data := range map[string]string{"ca.pem": string(ca.PEM), "lead": "old.example:7443\n", "term": "2\n"} {
		if err := os.WriteFile(filepath.Join(string(d), name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PutRecord(ctx, b, ca, Record{Term: 2, Address: "old.example:7443", WorkerID: "w-old"}, false); err != nil {
		t.Fatal(err)
	}
	if err := PutRecord(ctx, b, ca, Record{Term: 2, Address: "x:1", WorkerID: "w-x"}, false); err == nil {
		t.Fatal("a second record of one term was written")
	}
	if err := PutRecord(ctx, b, other, Record{Term: 9, Address: "evil.example:7443", WorkerID: "w-evil"}, false); err != nil {
		t.Fatal(err)
	}
	forged, _ := json.Marshal(Record{Term: 8, Address: "evil.example:7443", WorkerID: "w-evil", Sig: []byte("x")})
	b.Put(ctx, recordKey(8), forged, false)
	r, ok, err := HighestRecord(ctx, b, ca.Cert)
	if err != nil || !ok || r.Term != 2 {
		t.Fatalf("highest record: %+v %v %v, want term 2", r, ok, err)
	}

	if err := PutRecord(ctx, b, ca, Record{Term: 3, Address: "new.example:7443", WorkerID: "w-new"}, false); err != nil {
		t.Fatal(err)
	}
	if moved, err := follow(ctx, d, b); err != nil || !moved || d.Lead() != "new.example:7443" {
		t.Fatalf("follow: %v %v, lead %s", moved, err, d.Lead())
	}
	// A record below the term this worker has seen is never followed.
	os.WriteFile(filepath.Join(string(d), "term"), []byte("4\n"), 0o600)
	os.WriteFile(filepath.Join(string(d), "lead"), []byte("now.example:7443\n"), 0o600)
	if moved, err := follow(ctx, d, b); err != nil || moved {
		t.Fatalf("followed a term below the worker's: %v %v", moved, err)
	}
	if err := CheckTerm(d, 1); err == nil {
		t.Fatal("a lower term was accepted")
	}
}
