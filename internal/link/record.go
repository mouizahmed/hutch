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
	"time"

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
	cfg, ok, err := d.Bucket()
	if err != nil || !ok {
		return false, err // no bucket yet: nothing to follow
	}
	b, err := bucket.New(cfg)
	if err != nil {
		return false, err
	}
	return follow(ctx, d, b)
}

// Bucket is the bucket's settings as the lead last sent them, if it has.
func (d Dir) Bucket() (bucket.Config, bool, error) {
	var cfg bucket.Config
	data, err := os.ReadFile(d.path("bucket.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, err
	}
	return cfg, true, json.Unmarshal(data, &cfg)
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

// A machine's heartbeat, alive/<id> in the bucket ("lead" for the lead), is
// written every machines.lead_check while it runs, so a machine you say is
// down can be checked: one still writing to the bucket isn't.
type heartbeat struct {
	Every time.Duration `json:"every"`
}

func aliveKey(id string) string { return "alive/" + id }

// PutAlive writes machine id's heartbeat, to be written again within every.
func PutAlive(ctx context.Context, b bucket.Bucket, id string, every time.Duration) error {
	data, _ := json.Marshal(heartbeat{Every: every})
	return b.Put(ctx, aliveKey(id), data, false)
}

// StillAlive returns an error naming how recently machine id wrote its
// heartbeat, when that's within three of its intervals: it isn't down.
func StillAlive(ctx context.Context, b bucket.Bucket, id, name string) error {
	data, o, err := b.Fetch(ctx, aliveKey(id))
	if errors.Is(err, bucket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s's heartbeat in the bucket: %w", name, err)
	}
	var h heartbeat
	if json.Unmarshal(data, &h) != nil || h.Every <= 0 {
		return nil
	}
	now, err := bucketNow(ctx, b)
	if err != nil {
		return fmt.Errorf("reading the bucket's clock: %w", err)
	}
	age := now.Sub(o.Modified)
	if limit := 3 * h.Every; age < limit {
		return fmt.Errorf("%s wrote to the bucket %s ago, so it isn't down: it may be cut off from you and the other machines but still working. Stop it, or if it is down, wait %s and try again",
			name, age.Round(time.Second), (limit - age).Round(time.Second))
	}
	return nil
}

// bucketNow is the time by the bucket's own clock, the one that stamps
// heartbeats: this computer's clock may be off.
func bucketNow(ctx context.Context, b bucket.Bucket) (time.Time, error) {
	key := "alive/.now-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := b.Put(ctx, key, nil, false); err != nil {
		return time.Time{}, err
	}
	defer b.Delete(context.WithoutCancel(ctx), key)
	o, err := b.Head(ctx, key)
	return o.Modified, err
}
