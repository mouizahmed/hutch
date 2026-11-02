package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/link"
)

// leadLoop reads the lead record every machines.lead_check: this machine writes
// its term's record, claims a term after a restore, and stops leading when the
// record names a higher term.
func (s *Server) leadLoop(ctx context.Context) {
	for {
		if err := s.checkLead(ctx); err != nil && !errors.Is(err, errNotLead) {
			log.Printf("lead record: %v", err)
		}
		every := 30 * time.Second
		if cfg, err := s.settings(); err == nil {
			every = cfg.Duration("machines.lead_check")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

var errNotLead = errors.New("this machine no longer leads")

// notLead is the error for a machine whose lead record a higher term replaced.
func notLead(r *link.Record) error {
	return fmt.Errorf("%w: the machine at %s does (term %d)", errNotLead, r.Address, r.Term)
}

// checkLead returns errNotLead once another machine leads at a higher term. With
// workers and a bucket, it makes sure the bucket's lead record is this
// machine's: it writes its term's record, or after a restore (term 0) claims the
// term above the highest one in the bucket.
func (s *Server) checkLead(ctx context.Context) error {
	if r := s.superseded.Load(); r != nil {
		return notLead(r)
	}
	v := s.vault.Load()
	l, err := s.store.link()
	if v == nil || l == nil || err != nil {
		return err
	}
	s.mu.Lock()
	local := s.local
	s.mu.Unlock()
	if local == "" {
		return nil // until this machine's own worker attaches
	}
	s.leadMu.Lock()
	defer s.leadMu.Unlock()
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(l.address, strconv.FormatInt(cfg.Int("machine.link_port"), 10))
	h, ok, err := link.HighestRecord(ctx, v.bucket, l.ca.Cert)
	if err != nil {
		return err
	}
	mine := link.Record{Term: l.term, Address: addr, WorkerID: local}
	switch {
	case l.term == 0:
		mine.Term = 1
		if ok {
			mine.Term = h.Term + 1
		}
		// A term another machine took first is passed over: it's stale as soon
		// as it reads the higher one.
		for {
			err := link.PutRecord(ctx, v.bucket, l.ca, mine, false)
			if !errors.Is(err, bucket.ErrExists) {
				if err != nil {
					return err
				}
				break
			}
			mine.Term++
		}
		if err := s.store.setTerm(mine.Term); err != nil {
			return err
		}
		log.Printf("lead record: this machine leads at term %d (%s)", mine.Term, addr)
		s.store.event("", "lead", fmt.Sprintf("leads at term %d", mine.Term))
		return nil
	case ok && (h.Term > l.term || h.Term == l.term && h.WorkerID != local):
		s.stepDown(&h)
		return notLead(&h)
	case ok && h.Term == l.term:
		if h.Address != addr {
			return link.PutRecord(ctx, v.bucket, l.ca, mine, true)
		}
		return nil
	}
	// No valid record of this term: the first, or one that's malformed or
	// wrongly signed, which nobody acts on.
	err = link.PutRecord(ctx, v.bucket, l.ca, mine, false)
	if errors.Is(err, bucket.ErrExists) {
		err = link.PutRecord(ctx, v.bucket, l.ca, mine, true)
	}
	return err
}

// stepDown stops this machine leading: it takes no new boxes, backs up and
// collects nothing, and drops its linked workers, which follow the record.
func (s *Server) stepDown(r *link.Record) {
	if !s.superseded.CompareAndSwap(nil, r) {
		return
	}
	msg := fmt.Sprintf("the machine at %s leads at term %d, above this one's: this machine stopped leading (no new boxes, backups, or GC); its boxes keep running", r.Address, r.Term)
	log.Printf("lead record: %s", msg)
	s.store.event("", "lead", msg)
	s.notify("lead", "", "This machine stopped leading", msg)
	s.mu.Lock()
	for id, w := range s.workers {
		if id != s.local {
			w.sess.Close()
		}
	}
	s.mu.Unlock()
}

func (s *store) setTerm(term int64) error {
	_, err := s.exec(`UPDATE link SET term = ? WHERE id = 1`, term)
	return err
}

// backupName is a backup's name in the bucket: by term, then time.
func backupName(term int64, at time.Time) string {
	return fmt.Sprintf("server/backups/%010d-%016d", term, at.UnixMilli())
}

// parseBackup reads a backup's term and time; names from before terms are term 0.
func parseBackup(key string) (term int64, at time.Time, ok bool) {
	rest := key[len("server/backups/"):]
	t, ms := "0", rest
	if i := len(rest) - 17; i > 0 && rest[i] == '-' {
		t, ms = rest[:i], rest[i+1:]
	}
	term, err1 := strconv.ParseInt(t, 10, 64)
	n, err2 := strconv.ParseInt(ms, 10, 64)
	return term, time.UnixMilli(n).UTC(), err1 == nil && err2 == nil
}

// newerBackup orders backups: the higher term, then the later time.
func newerBackup(a, b string) bool {
	at, att, _ := parseBackup(a)
	bt, btt, _ := parseBackup(b)
	return at > bt || at == bt && att.After(btt)
}
