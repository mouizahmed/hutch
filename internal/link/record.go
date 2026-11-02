package link

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/pki"
)

// A lead record, lead/<term> in the bucket, says where the lead of a term is.
// Only a lead holds the link CA's key that signs it, so one written by anyone
// else is ignored; the link's certificates still decide whom a worker trusts.
type Record struct {
	Term     int64  `json:"term"`
	Address  string `json:"address"`
	WorkerID string `json:"worker_id"`
	Sig      []byte `json:"sig,omitempty"`
}

const recordPrefix = "lead/"

func recordKey(term int64) string { return fmt.Sprintf("%s%020d", recordPrefix, term) }

func (r Record) digest() []byte {
	b, _ := json.Marshal(Record{Term: r.Term, Address: r.Address, WorkerID: r.WorkerID})
	sum := sha256.Sum256(b)
	return sum[:]
}

// PutRecord signs r with the CA's key and writes it. With replace it may
// overwrite the term's record; otherwise a term already taken is ErrExists.
func PutRecord(ctx context.Context, b bucket.Bucket, ca *pki.CA, r Record, replace bool) error {
	sig, err := ecdsa.SignASN1(rand.Reader, ca.Key, r.digest())
	if err != nil {
		return err
	}
	r.Sig = sig
	data, _ := json.Marshal(r)
	return b.Put(ctx, recordKey(r.Term), data, !replace)
}

// HighestRecord is the validly signed record of the highest term, and whether
// there is one.
func HighestRecord(ctx context.Context, b bucket.Bucket, ca *x509.Certificate) (Record, bool, error) {
	pub, ok := ca.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return Record{}, false, errors.New("the link CA's key isn't ECDSA")
	}
	objs, err := b.List(ctx, recordPrefix)
	if err != nil {
		return Record{}, false, err
	}
	var best Record
	found := false
	for _, o := range objs {
		term, err := strconv.ParseInt(strings.TrimPrefix(o.Key, recordPrefix), 10, 64)
		if err != nil || (found && term <= best.Term) {
			continue
		}
		data, err := b.Get(ctx, o.Key)
		if err != nil {
			return Record{}, false, err
		}
		var r Record
		if json.Unmarshal(data, &r) != nil || r.Term != term || !ecdsa.VerifyASN1(pub, r.digest(), r.Sig) {
			continue
		}
		best, found = r, true
	}
	return best, found, nil
}

// SaveBucket keeps the bucket's configuration, so a worker without a lead can
// read the lead record.
func (d Dir) SaveBucket(cfg bucket.Config) error {
	data, _ := json.Marshal(cfg)
	if old, err := os.ReadFile(d.path("bucket.json")); err == nil && string(old) == string(data) {
		return nil
	}
	tmp := d.path("bucket.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path("bucket.json"))
}

// Follow reads the lead record and, when it names a higher term than this
// worker has seen at another address, dials that address from then on. It
// reports whether the address changed.
func (d Dir) Follow(ctx context.Context) (bool, error) {
	data, err := os.ReadFile(d.path("bucket.json"))
	if err != nil {
		return false, nil // no bucket yet: nothing to follow
	}
	var cfg bucket.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, err
	}
	b, err := bucket.New(cfg)
	if err != nil {
		return false, err
	}
	return follow(ctx, d, b)
}

func follow(ctx context.Context, d Dir, b bucket.Bucket) (bool, error) {
	caPEM, err := os.ReadFile(d.path("ca.pem"))
	if err != nil {
		return false, err
	}
	ca, err := pki.ParseCert(caPEM)
	if err != nil {
		return false, err
	}
	r, ok, err := HighestRecord(ctx, b, ca)
	if err != nil || !ok || r.Term < d.Term() || r.Address == d.Lead() {
		return false, err
	}
	return true, d.SetLead(r.Address)
}

// Term is the highest lead term this worker has seen.
func (d Dir) Term() int64 {
	b, _ := os.ReadFile(d.path("term"))
	t, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return t
}
