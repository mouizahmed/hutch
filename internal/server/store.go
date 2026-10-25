package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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
CREATE TABLE IF NOT EXISTS threads (
	box_id  TEXT NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
	n       INTEGER NOT NULL,
	agent   TEXT NOT NULL,
	title   TEXT NOT NULL,
	created TIMESTAMP NOT NULL,
	PRIMARY KEY (box_id, n)
);
CREATE TABLE IF NOT EXISTS tokens (
	hash    TEXT PRIMARY KEY,
	created TIMESTAMP NOT NULL,
	used    TIMESTAMP
);
CREATE TABLE IF NOT EXISTS projects (
	id      TEXT PRIMARY KEY,
	slug    TEXT NOT NULL UNIQUE,
	created TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS templates (
	project_id TEXT NOT NULL REFERENCES projects(id),
	version    INTEGER NOT NULL,
	commit_sha TEXT NOT NULL,
	box_id     TEXT NOT NULL,
	vcpus      INTEGER NOT NULL,
	mem_mib    INTEGER NOT NULL,
	created    TIMESTAMP NOT NULL,
	PRIMARY KEY (project_id, version)
);
-- What the server knows about a box beyond the worker's report.
CREATE TABLE IF NOT EXISTS box_meta (
	box_id     TEXT PRIMARY KEY REFERENCES boxes(id) ON DELETE CASCADE,
	project_id TEXT NOT NULL,
	kind       TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	verified   INTEGER NOT NULL DEFAULT 0
);
-- The user layer: "config" and one "credentials/<adapter>[/mcp]" part per login.
CREATE TABLE IF NOT EXISTS user_layer (
	part    TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	content BLOB NOT NULL,
	updated TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS configs (
	name    TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	content TEXT NOT NULL,
	updated TIMESTAMP NOT NULL
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
	err := s.db.QueryRow(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, '') FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id WHERE id = ?`, id).
		Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("no box %q", id)
	}
	return b, err
}

func (s *store) boxes() ([]api.Box, error) {
	rows, err := s.db.Query(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, '') FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []api.Box
	for rows.Next() {
		var b api.Box
		if err := rows.Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind); err != nil {
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

func (s *store) addThread(t api.Thread) error {
	_, err := s.db.Exec(`INSERT INTO threads (box_id, n, agent, title, created) VALUES (?, ?, ?, ?, ?)`,
		t.Box, t.N, t.Agent, t.Title, t.Created)
	return err
}

func (s *store) threads() (map[string][]api.Thread, error) {
	rows, err := s.db.Query(`SELECT box_id, n, agent, title, created FROM threads ORDER BY box_id, n`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byBox := map[string][]api.Thread{}
	for rows.Next() {
		var t api.Thread
		if err := rows.Scan(&t.Box, &t.N, &t.Agent, &t.Title, &t.Created); err != nil {
			return nil, err
		}
		byBox[t.Box] = append(byBox[t.Box], t)
	}
	return byBox, rows.Err()
}

func (s *store) config(name string) (api.ConfigDoc, error) {
	d := api.ConfigDoc{Name: name}
	err := s.db.QueryRow(`SELECT version, content FROM configs WHERE name = ?`, name).Scan(&d.Version, &d.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return d, nil
	}
	return d, err
}

// putConfig writes a document if it is still at base; otherwise it returns the
// current one and conflict.
func (s *store) putConfig(name string, base int64, content string) (api.ConfigDoc, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return api.ConfigDoc{}, false, err
	}
	defer tx.Rollback()
	cur := api.ConfigDoc{Name: name}
	err = tx.QueryRow(`SELECT version, content FROM configs WHERE name = ?`, name).Scan(&cur.Version, &cur.Content)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cur, false, err
	}
	if cur.Version != base {
		return cur, true, nil
	}
	next := api.ConfigDoc{Name: name, Version: base + 1, Content: content}
	_, err = tx.Exec(`INSERT INTO configs (name, version, content, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET version = excluded.version, content = excluded.content, updated = excluded.updated`,
		name, next.Version, content, time.Now().UTC())
	if err != nil {
		return cur, false, err
	}
	return next, false, tx.Commit()
}

type boxMeta struct {
	project, kind, commit string
	verified              bool
}

func (s *store) putMeta(box string, m boxMeta) error {
	_, err := s.db.Exec(`INSERT INTO box_meta (box_id, project_id, kind, commit_sha, verified) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(box_id) DO UPDATE SET project_id = excluded.project_id, kind = excluded.kind,
			commit_sha = excluded.commit_sha, verified = excluded.verified`,
		box, m.project, m.kind, m.commit, m.verified)
	return err
}

func (s *store) meta(box string) (boxMeta, error) {
	var m boxMeta
	err := s.db.QueryRow(`SELECT project_id, kind, commit_sha, verified FROM box_meta WHERE box_id = ?`, box).
		Scan(&m.project, &m.kind, &m.commit, &m.verified)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	return m, err
}

func (s *store) project(id string) (api.Project, error) {
	p := api.Project{ID: id}
	err := s.db.QueryRow(`SELECT slug FROM projects WHERE id = ?`, id).Scan(&p.Slug)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("no project %s on this server (run hutch init)", id)
	}
	return p, err
}

// addProject registers a project under the first free slug from base, base-2, ...
func (s *store) addProject(id, base string) (api.Project, error) {
	if p, err := s.project(id); err == nil {
		return p, nil
	}
	for n := 1; ; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		_, err := s.db.Exec(`INSERT INTO projects (id, slug, created) VALUES (?, ?, ?)`, id, slug, time.Now().UTC())
		if err == nil {
			return api.Project{ID: id, Slug: slug}, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") || n > 100 {
			return api.Project{}, err
		}
	}
}

func (s *store) addTemplate(t api.Template) error {
	_, err := s.db.Exec(`INSERT INTO templates (project_id, version, commit_sha, box_id, vcpus, mem_mib, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Project, t.Version, t.Commit, t.Box, t.VCPUs, t.MemMiB, t.Created)
	return err
}

// templates lists a project's versions, newest first.
func (s *store) templates(project string) ([]api.Template, error) {
	rows, err := s.db.Query(`SELECT version, commit_sha, box_id, vcpus, mem_mib, created FROM templates WHERE project_id = ? ORDER BY version DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []api.Template
	for rows.Next() {
		t := api.Template{Project: project}
		if err := rows.Scan(&t.Version, &t.Commit, &t.Box, &t.VCPUs, &t.MemMiB, &t.Created); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

type layerPart struct {
	version int64
	content []byte
	updated time.Time
}

func (s *store) layerPart(part string) (layerPart, error) {
	var p layerPart
	err := s.db.QueryRow(`SELECT version, content, updated FROM user_layer WHERE part = ?`, part).Scan(&p.version, &p.content, &p.updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// layerParts lists the parts whose names start with prefix.
func (s *store) layerParts(prefix string) (map[string]layerPart, error) {
	rows, err := s.db.Query(`SELECT part, version, content, updated FROM user_layer WHERE part LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]layerPart{}
	for rows.Next() {
		var name string
		var p layerPart
		if err := rows.Scan(&name, &p.version, &p.content, &p.updated); err != nil {
			return nil, err
		}
		out[name] = p
	}
	return out, rows.Err()
}

// putLayerPart writes a part if it is still at base (base < 0: unconditionally).
func (s *store) putLayerPart(part string, base int64, content []byte) (int64, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var cur int64
	err = tx.QueryRow(`SELECT version FROM user_layer WHERE part = ?`, part).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	if base >= 0 && cur != base {
		return cur, true, nil
	}
	_, err = tx.Exec(`INSERT INTO user_layer (part, version, content, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(part) DO UPDATE SET version = excluded.version, content = excluded.content, updated = excluded.updated`,
		part, cur+1, content, time.Now().UTC())
	if err != nil {
		return 0, false, err
	}
	return cur + 1, false, tx.Commit()
}

func (s *store) deleteLayerPart(part string) error {
	_, err := s.db.Exec(`DELETE FROM user_layer WHERE part = ?`, part)
	return err
}
