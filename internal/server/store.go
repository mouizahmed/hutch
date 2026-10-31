package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
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
-- The logins each box was granted, for pushing refreshed credentials to it.
CREATE TABLE IF NOT EXISTS box_credentials (
	box_id      TEXT PRIMARY KEY REFERENCES boxes(id) ON DELETE CASCADE,
	credentials TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS box_git (
	box_id TEXT PRIMARY KEY REFERENCES boxes(id) ON DELETE CASCADE,
	branch TEXT NOT NULL,
	base   TEXT NOT NULL
);
-- A template version built again for another snapshot class, as its own box.
CREATE TABLE IF NOT EXISTS template_builds (
	project_id TEXT NOT NULL,
	version    INTEGER NOT NULL,
	class      TEXT NOT NULL,
	box_id     TEXT NOT NULL,
	PRIMARY KEY (project_id, version, class)
);
-- Workers holding a template version besides the one in boxes (boxes.worker_id).
CREATE TABLE IF NOT EXISTS version_copies (
	box_id    TEXT NOT NULL,
	worker_id TEXT NOT NULL,
	PRIMARY KEY (box_id, worker_id)
);
-- Files from a project's .hutch/uploads.txt, encrypted with the server's secrets key.
CREATE TABLE IF NOT EXISTS project_secrets (
	project_id TEXT PRIMARY KEY REFERENCES projects(id),
	digest     TEXT NOT NULL,
	content    BLOB NOT NULL,
	updated    TIMESTAMP NOT NULL
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
	// userKey seals the user layer, so a copy of the database alone doesn't expose credentials.
	userKey durable.Key
	// onChange is called after every write, for events.
	onChange func()
}

func (s *store) exec(query string, args ...any) (sql.Result, error) {
	res, err := s.db.Exec(query, args...)
	if err == nil {
		s.changed()
	}
	return res, err
}

func (s *store) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// migrations add columns to tables from earlier versions; each may already be done.
var migrations = []string{
	`ALTER TABLE box_meta ADD COLUMN secrets TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE templates ADD COLUMN secrets TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN peaks TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE projects ADD COLUMN confirmed TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE box_meta ADD COLUMN version INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE boxes ADD COLUMN state_since TIMESTAMP`,
	`ALTER TABLE workers ADD COLUMN cpus INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workers ADD COLUMN mem_mib INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workers ADD COLUMN arch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workers ADD COLUMN disk_bytes INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workers ADD COLUMN gone INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE workers ADD COLUMN draining INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE templates ADD COLUMN params TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE boxes ADD COLUMN path TEXT NOT NULL DEFAULT ''`,
	// A box's threads, as it last reported them: its tmux is their record.
	`ALTER TABLE boxes ADD COLUMN threads TEXT NOT NULL DEFAULT ''`,
	`DROP TABLE IF EXISTS threads`,
	`UPDATE configs SET name = 'machine' WHERE name = 'server'`,
	`ALTER TABLE box_meta ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE templates ADD COLUMN image TEXT NOT NULL DEFAULT ''`,
}

func openStore(path string, userKey durable.Key) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	return &store{db: db, userKey: userKey}, nil
}

// changes counts the rows the server has changed since it started.
func (s *store) changes() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT total_changes()`).Scan(&n)
	return n, err
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ensureWorker records the worker on this machine, keeping its boxes if it is known.
func (s *store) ensureWorker(id, name string) error {
	now := time.Now().UTC()
	_, err := s.exec(`INSERT INTO workers (id, name, created, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, gone = 0`, id, name, now, now)
	return err
}

func (s *store) setDraining(id string, on bool) error {
	res, err := s.exec(`UPDATE workers SET draining = ? WHERE id = ?`, on, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no worker %s", id)
	}
	return nil
}

func (s *store) setWorkerGone(id string) error {
	_, err := s.exec(`UPDATE workers SET gone = 1 WHERE id = ?`, id)
	return err
}

// workerSeen records a worker's capacity as of its connection, and when it was last seen.
func (s *store) workerSeen(id string, w api.Worker) error {
	_, err := s.exec(`UPDATE workers SET class = ?, version = ?, cpus = ?, mem_mib = ?, arch = ?, disk_bytes = ?, last_seen = ? WHERE id = ?`,
		w.Class, w.Version, w.CPUs, w.MemMiB, w.Arch, w.Disk.Total, time.Now().UTC(), id)
	return err
}

func (s *store) workers() ([]api.Worker, error) {
	rows, err := s.db.Query(`SELECT id, name, class, version, last_seen, cpus, mem_mib, arch, disk_bytes, gone, draining FROM workers ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []api.Worker
	for rows.Next() {
		var w api.Worker
		if err := rows.Scan(&w.ID, &w.Name, &w.Class, &w.Version, &w.LastSeen, &w.CPUs, &w.MemMiB, &w.Arch, &w.Disk.Total, &w.Gone, &w.Draining); err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, rows.Err()
}

func (s *store) putBox(b api.Box) error {
	_, err := s.exec(`
		INSERT INTO boxes (id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET worker_id = excluded.worker_id, state = excluded.state, generation = excluded.generation,
			vcpus = excluded.vcpus, mem_mib = excluded.mem_mib,
			class = excluded.class, reach_ip = excluded.reach_ip, updated = excluded.updated,
			state_since = CASE WHEN boxes.state = excluded.state THEN boxes.state_since ELSE excluded.updated END`,
		b.ID, b.Worker, b.State, b.Image, b.VCPUs, b.MemMiB, b.Generation, b.Class, b.ReachIP, b.Created, time.Now().UTC())
	return err
}

// removed reports whether a box's last event is its removal.
func (s *store) removed(id string) bool {
	var kind string
	s.db.QueryRow(`SELECT kind FROM events WHERE box_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&kind)
	return kind == api.BoxRm
}

// versionWorkers lists the workers holding a template version.
func (s *store) versionWorkers(box string) ([]string, error) {
	rows, err := s.db.Query(`SELECT worker_id FROM boxes WHERE id = ? UNION SELECT worker_id FROM version_copies WHERE box_id = ?`, box, box)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, rows.Err()
}

func (s *store) addVersionCopy(box, worker string) error {
	_, err := s.exec(`INSERT OR IGNORE INTO version_copies (box_id, worker_id) VALUES (?, ?)`, box, worker)
	return err
}

func (s *store) deleteVersionCopy(box, worker string) error {
	_, err := s.exec(`DELETE FROM version_copies WHERE box_id = ? AND worker_id = ?`, box, worker)
	return err
}

// statesSince returns when each box entered its current state.
func (s *store) statesSince() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT id, COALESCE(state_since, updated) FROM boxes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// deleteBox forgets a box, saying why in its events; a worker's report about it
// sent before then is ignored (removed), so it can't come back without its meta.
func (s *store) deleteBox(id, why string) error {
	if _, err := s.exec(`DELETE FROM version_copies WHERE box_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.exec(`DELETE FROM boxes WHERE id = ?`, id); err != nil {
		return err
	}
	s.event(id, api.BoxRm, why)
	return nil
}

func (s *store) box(id string) (api.Box, error) {
	var b api.Box
	err := s.db.QueryRow(`SELECT id, worker_id, state, boxes.image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, ''), COALESCE(g.branch, ''), COALESCE(g.base, ''), path, threads
		FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id LEFT JOIN box_git g ON g.box_id = boxes.id WHERE id = ?`, id).
		Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind, &b.Branch, &b.Base, &b.Path, threadsJSON{&b.Threads})
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("no box %q", id)
	}
	return b, err
}

func (s *store) boxes() ([]api.Box, error) {
	rows, err := s.db.Query(`SELECT id, worker_id, state, boxes.image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, ''), COALESCE(g.branch, ''), COALESCE(g.base, ''), path, threads
		FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id LEFT JOIN box_git g ON g.box_id = boxes.id ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []api.Box
	for rows.Next() {
		var b api.Box
		if err := rows.Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind, &b.Branch, &b.Base, &b.Path, threadsJSON{&b.Threads}); err != nil {
			return nil, err
		}
		bs = append(bs, b)
	}
	return bs, rows.Err()
}

// syncWorkerBoxes replaces what the server knows about a worker's boxes and
// template version copies with its report.
func (s *store) syncWorkerBoxes(workerID string, boxes []api.Box, copies []string) error {
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
	if _, err := tx.Exec(`DELETE FROM version_copies WHERE worker_id = ?`, workerID); err != nil {
		return err
	}
	for _, id := range copies {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO version_copies (box_id, worker_id) VALUES (?, ?)`, id, workerID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed()
	for _, b := range boxes {
		b.Worker = workerID
		if err := s.putBox(b); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) event(boxID, kind, msg string) {
	s.exec(`INSERT INTO events (ts, box_id, kind, message) VALUES (?, ?, ?, ?)`, time.Now().UTC(), boxID, kind, msg)
}

// setThreads caches a box's threads as it reported them.
func (s *store) setThreads(box string, threads []api.Thread) error {
	data, err := json.Marshal(threads)
	if err != nil {
		return err
	}
	_, err = s.exec(`UPDATE boxes SET threads = ? WHERE id = ? AND threads != ?`, string(data), box, string(data))
	return err
}

// threadsJSON scans a box's cached threads.
type threadsJSON struct{ t *[]api.Thread }

func (j threadsJSON) Scan(v any) error {
	var data []byte
	switch v := v.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	}
	if len(data) == 0 {
		*j.t = nil
		return nil
	}
	return json.Unmarshal(data, j.t)
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
	if err := tx.Commit(); err != nil {
		return api.ConfigDoc{}, false, err
	}
	s.changed()
	return next, false, nil
}

type boxMeta struct {
	project, kind, commit string
	verified              bool
	secrets               string // digest of the project secrets a template box was built with
	version               int    // the template version a box was forked from
	image                 string // a template box's image: what it was built from, pinned
}

func (s *store) putMeta(box string, m boxMeta) error {
	_, err := s.exec(`INSERT INTO box_meta (box_id, project_id, kind, commit_sha, verified, secrets, version) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(box_id) DO UPDATE SET project_id = excluded.project_id, kind = excluded.kind,
			commit_sha = excluded.commit_sha, verified = excluded.verified, secrets = excluded.secrets, version = excluded.version`,
		box, m.project, m.kind, m.commit, m.verified, m.secrets, m.version)
	return err
}

// setImage records what a template box's image was built from.
func (s *store) setImage(box, image string) error {
	_, err := s.exec(`UPDATE box_meta SET image = ? WHERE box_id = ?`, image, box)
	return err
}

func (s *store) meta(box string) (boxMeta, error) {
	var m boxMeta
	err := s.db.QueryRow(`SELECT project_id, kind, commit_sha, verified, secrets, image FROM box_meta WHERE box_id = ?`, box).
		Scan(&m.project, &m.kind, &m.commit, &m.verified, &m.secrets, &m.image)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	return m, err
}

func (s *store) project(id string) (api.Project, error) {
	p := api.Project{ID: id}
	var peaks string
	err := s.db.QueryRow(`SELECT slug, peaks, confirmed FROM projects WHERE id = ?`, id).Scan(&p.Slug, &peaks, &p.Confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("no project %s on the machine (run hutch init)", id)
	}
	if err == nil && peaks != "" {
		p.Peaks = &api.Peaks{}
		err = json.Unmarshal([]byte(peaks), p.Peaks)
	}
	return p, err
}

func (s *store) setConfirmed(id, settings string) error {
	_, err := s.exec(`UPDATE projects SET confirmed = ? WHERE id = ?`, settings, id)
	return err
}

func (s *store) setPeaks(id string, p api.Peaks) error {
	data, _ := json.Marshal(p)
	_, err := s.exec(`UPDATE projects SET peaks = ? WHERE id = ?`, string(data), id)
	return err
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
		_, err := s.exec(`INSERT INTO projects (id, slug, created) VALUES (?, ?, ?)`, id, slug, time.Now().UTC())
		if err == nil {
			return api.Project{ID: id, Slug: slug}, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") || n > 100 {
			return api.Project{}, err
		}
	}
}

func (s *store) addTemplate(t api.Template) error {
	_, err := s.exec(`INSERT INTO templates (project_id, version, commit_sha, box_id, vcpus, mem_mib, created, secrets, image) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Project, t.Version, t.Commit, t.Box, t.VCPUs, t.MemMiB, t.Created, t.Secrets, t.Image)
	return err
}

// templates lists a project's versions, newest first.
func (s *store) templates(project string) ([]api.Template, error) {
	rows, err := s.db.Query(`SELECT version, commit_sha, box_id, vcpus, mem_mib, created, secrets, image FROM templates WHERE project_id = ? ORDER BY version DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []api.Template
	for rows.Next() {
		t := api.Template{Project: project}
		if err := rows.Scan(&t.Version, &t.Commit, &t.Box, &t.VCPUs, &t.MemMiB, &t.Created, &t.Secrets, &t.Image); err != nil {
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
	if err != nil {
		return p, err
	}
	p.content, err = s.userKey.Open("user/"+part, p.content)
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
		if p.content, err = s.userKey.Open("user/"+name, p.content); err != nil {
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
		part, cur+1, s.userKey.Seal("user/"+part, content), time.Now().UTC())
	if err != nil {
		return 0, false, err
	}
	return cur + 1, false, tx.Commit()
}

func (s *store) deleteLayerPart(part string) error {
	_, err := s.exec(`DELETE FROM user_layer WHERE part = ?`, part)
	return err
}

// putLayerParts commits related encrypted parts together. In particular, a
// refresh's credential files and proxy aliases must survive or roll back as one.
func (s *store) putLayerParts(parts map[string][]byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for part, content := range parts {
		if _, err := tx.Exec(`INSERT INTO user_layer (part, version, content, updated) VALUES (?, 1, ?, ?)
			ON CONFLICT(part) DO UPDATE SET version = user_layer.version + 1, content = excluded.content, updated = excluded.updated`,
			part, s.userKey.Seal("user/"+part, content), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) setBoxCredentials(box string, creds []string) error {
	data, _ := json.Marshal(creds)
	_, err := s.exec(`INSERT INTO box_credentials (box_id, credentials) VALUES (?, ?)
		ON CONFLICT(box_id) DO UPDATE SET credentials = excluded.credentials`, box, string(data))
	return err
}

func (s *store) boxCredentials(box string) ([]string, error) {
	var data string
	err := s.db.QueryRow(`SELECT credentials FROM box_credentials WHERE box_id = ?`, box).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var creds []string
	return creds, json.Unmarshal([]byte(data), &creds)
}

// allBoxCredentials returns every box's credential grants.
func (s *store) allBoxCredentials() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT box_id, credentials FROM box_credentials`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var box, data string
		if err := rows.Scan(&box, &data); err != nil {
			return nil, err
		}
		var creds []string
		if err := json.Unmarshal([]byte(data), &creds); err != nil {
			return nil, err
		}
		out[box] = creds
	}
	return out, rows.Err()
}

func (s *store) setBoxPath(box, path string) error {
	_, err := s.exec(`UPDATE boxes SET path = ? WHERE id = ?`, path, box)
	return err
}

// setBoxGit records a box's branch, as checked out now, and the default branch it
// is compared with.
func (s *store) setBoxGit(box, branch, base string) error {
	_, err := s.exec(`INSERT INTO box_git (box_id, branch, base) VALUES (?, ?, ?)
		ON CONFLICT(box_id) DO UPDATE SET branch = excluded.branch, base = excluded.base`, box, branch, base)
	return err
}

func (s *store) projectSecrets(project string) (digest string, content []byte, err error) {
	err = s.db.QueryRow(`SELECT digest, content FROM project_secrets WHERE project_id = ?`, project).Scan(&digest, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	return digest, content, err
}

func (s *store) putProjectSecrets(project, digest string, content []byte) error {
	_, err := s.exec(`INSERT INTO project_secrets (project_id, digest, content, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET digest = excluded.digest, content = excluded.content, updated = excluded.updated`,
		project, digest, content, time.Now().UTC())
	return err
}

// versionsInUse returns the template versions a project's boxes were forked from,
// and the commits of boxes forked before versions were recorded.
func (s *store) versionsInUse(project string) (map[int]bool, map[string]bool, error) {
	rows, err := s.db.Query(`SELECT version, commit_sha FROM box_meta WHERE project_id = ? AND kind != ?`, project, api.BoxVersion)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	versions, commits := map[int]bool{}, map[string]bool{}
	for rows.Next() {
		var v int
		var c string
		if err := rows.Scan(&v, &c); err != nil {
			return nil, nil, err
		}
		if v > 0 {
			versions[v] = true
		} else if c != "" {
			commits[c] = true
		}
	}
	return versions, commits, rows.Err()
}

func (s *store) deleteTemplate(project string, version int) error {
	if _, err := s.exec(`DELETE FROM template_builds WHERE project_id = ? AND version = ?`, project, version); err != nil {
		return err
	}
	_, err := s.exec(`DELETE FROM templates WHERE project_id = ? AND version = ?`, project, version)
	return err
}

// setTemplateParams keeps how a version was built, to build it again for another class.
func (s *store) setTemplateParams(project string, version int, params string) error {
	_, err := s.exec(`UPDATE templates SET params = ? WHERE project_id = ? AND version = ?`, params, project, version)
	return err
}

func (s *store) templateParams(project string, version int) (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT params FROM templates WHERE project_id = ? AND version = ?`, project, version).Scan(&p)
	return p, err
}

type templateBuild struct{ class, box string }

// templateBuilds lists a version's builds: the original and any for other classes.
func (s *store) templateBuilds(project string, version int) ([]templateBuild, error) {
	rows, err := s.db.Query(`SELECT b.class, t.box_id FROM templates t JOIN boxes b ON b.id = t.box_id WHERE t.project_id = ? AND t.version = ?
		UNION ALL SELECT class, box_id FROM template_builds WHERE project_id = ? AND version = ?`, project, version, project, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []templateBuild
	for rows.Next() {
		var b templateBuild
		if err := rows.Scan(&b.class, &b.box); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *store) addTemplateBuild(project string, version int, class, box string) error {
	_, err := s.exec(`INSERT OR REPLACE INTO template_builds (project_id, version, class, box_id) VALUES (?, ?, ?, ?)`, project, version, class, box)
	return err
}

// buildVersion finds which version and class a per-class build box is.
func (s *store) buildVersion(box string) (project string, version int, class string, err error) {
	err = s.db.QueryRow(`SELECT project_id, version, class FROM template_builds WHERE box_id = ?`, box).Scan(&project, &version, &class)
	return
}

func (s *store) projects() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteProject forgets a project and its templates and secrets.
func (s *store) deleteProject(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM templates WHERE project_id = ?`,
		`DELETE FROM project_secrets WHERE project_id = ?`,
		`DELETE FROM projects WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.changed()
	return nil
}
