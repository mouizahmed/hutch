package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mouizahmed/hutch/internal/api"
)

const schema = `
CREATE TABLE IF NOT EXISTS workers (
	id        TEXT PRIMARY KEY,
	name      TEXT NOT NULL,
	class     TEXT NOT NULL DEFAULT '',
	version   TEXT NOT NULL DEFAULT '',
	created   TIMESTAMP NOT NULL,
	last_seen TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS boxes (
	id         TEXT PRIMARY KEY,
	worker_id  TEXT NOT NULL REFERENCES workers(id),
	state      TEXT NOT NULL,
	image      TEXT NOT NULL,
	vcpus      INTEGER NOT NULL,
	mem_mib    INTEGER NOT NULL,
	generation INTEGER NOT NULL,
	class      TEXT NOT NULL,
	reach_ip   TEXT NOT NULL DEFAULT '',
	created    TIMESTAMP NOT NULL,
	updated    TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	hash    TEXT PRIMARY KEY,
	created TIMESTAMP NOT NULL,
	used    TIMESTAMP
);
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY,
	ts      TIMESTAMP NOT NULL,
	box_id  TEXT NOT NULL DEFAULT '',
	kind    TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT ''
);
`

type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &store{db: db}, nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *store) addToken(secret string) error {
	_, err := s.db.Exec(`INSERT INTO tokens (hash, created) VALUES (?, ?)`, hashSecret(secret), time.Now().UTC())
	return err
}

// useToken marks a join token used; each token enrolls exactly one worker.
func (s *store) useToken(secret string) error {
	res, err := s.db.Exec(`UPDATE tokens SET used = ? WHERE hash = ? AND used IS NULL`, time.Now().UTC(), hashSecret(secret))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("invalid or already used join token")
	}
	return nil
}

func (s *store) addWorker(id, name string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO workers (id, name, created, last_seen) VALUES (?, ?, ?, ?)`, id, name, now, now)
	return err
}

func (s *store) workerSeen(id, class, version string) error {
	_, err := s.db.Exec(`UPDATE workers SET class = ?, version = ?, last_seen = ? WHERE id = ?`, class, version, time.Now().UTC(), id)
	return err
}

func (s *store) workers() ([]api.Worker, error) {
	rows, err := s.db.Query(`SELECT id, name, class, version, last_seen FROM workers ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []api.Worker
	for rows.Next() {
		var w api.Worker
		if err := rows.Scan(&w.ID, &w.Name, &w.Class, &w.Version, &w.LastSeen); err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, rows.Err()
}

func (s *store) putBox(b api.Box) error {
	_, err := s.db.Exec(`
		INSERT INTO boxes (id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET worker_id = excluded.worker_id, state = excluded.state, generation = excluded.generation,
			class = excluded.class, reach_ip = excluded.reach_ip, updated = excluded.updated`,
		b.ID, b.Worker, b.State, b.Image, b.VCPUs, b.MemMiB, b.Generation, b.Class, b.ReachIP, b.Created, time.Now().UTC())
	return err
}

func (s *store) deleteBox(id string) error {
	_, err := s.db.Exec(`DELETE FROM boxes WHERE id = ?`, id)
	return err
}

func (s *store) box(id string) (api.Box, error) {
	var b api.Box
	err := s.db.QueryRow(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created FROM boxes WHERE id = ?`, id).
		Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("no box %q", id)
	}
	return b, err
}

func (s *store) boxes() ([]api.Box, error) {
	rows, err := s.db.Query(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created FROM boxes ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []api.Box
	for rows.Next() {
		var b api.Box
		if err := rows.Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created); err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}

// syncWorkerBoxes replaces what the server knows about a worker's boxes with its report.
func (s *store) syncWorkerBoxes(workerID string, boxes []api.Box) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for _, b := range boxes {
		keep[b.ID] = true
	}
	rows, err := tx.Query(`SELECT id FROM boxes WHERE worker_id = ?`, workerID)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := tx.Exec(`DELETE FROM boxes WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, b := range boxes {
		b.Worker = workerID
		if err := s.putBox(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) event(boxID, kind, msg string) {
	s.db.Exec(`INSERT INTO events (ts, box_id, kind, message) VALUES (?, ?, ?, ?)`, time.Now().UTC(), boxID, kind, msg)
}
