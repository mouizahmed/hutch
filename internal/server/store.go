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
-- The cost ledger: what Hutch estimates each box (and, with box '', the fleet) cost per day.
CREATE TABLE IF NOT EXISTS costs (
	day      TEXT NOT NULL,
	box_id   TEXT NOT NULL,
	category TEXT NOT NULL,
	usd      REAL NOT NULL,
	PRIMARY KEY (day, box_id, category)
);
-- What the provider billed per day, next to the estimate.
CREATE TABLE IF NOT EXISTS billed (
	day      TEXT NOT NULL,
	category TEXT NOT NULL,
	usd      REAL NOT NULL,
	PRIMARY KEY (day, category)
);
-- Small facts the server keeps across restarts.
CREATE TABLE IF NOT EXISTS state (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
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
}

// migrations add columns to tables from earlier versions; each may already be done.
var migrations = []string{
	`ALTER TABLE threads ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE threads ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE threads ADD COLUMN autonomy TEXT NOT NULL DEFAULT 'full'`,
	`ALTER TABLE threads ADD COLUMN renamed INTEGER NOT NULL DEFAULT 0`,
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
	`ALTER TABLE workers ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workers ADD COLUMN stopped_at TIMESTAMP`,
	`ALTER TABLE workers ADD COLUMN gone INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tokens ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE workers ADD COLUMN draining INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE templates ADD COLUMN params TEXT NOT NULL DEFAULT ''`,
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

// addToken stores a join token; nodeID links the worker it enrolls to the cloud
// node it runs on ("" for a persistent worker).
func (s *store) addToken(secret, nodeID string) error {
	_, err := s.db.Exec(`INSERT INTO tokens (hash, created, node_id) VALUES (?, ?, ?)`, hashSecret(secret), time.Now().UTC(), nodeID)
	return err
}

// useToken marks a join token used, returning its node; each token enrolls exactly one worker.
func (s *store) useToken(secret string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var node string
	err = tx.QueryRow(`SELECT node_id FROM tokens WHERE hash = ? AND used IS NULL`, hashSecret(secret)).Scan(&node)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("invalid or already used join token")
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE tokens SET used = ? WHERE hash = ?`, time.Now().UTC(), hashSecret(secret)); err != nil {
		return "", err
	}
	return node, tx.Commit()
}

// setTokenNode links an unused token to the node it was given to.
func (s *store) setTokenNode(secret, nodeID string) error {
	_, err := s.db.Exec(`UPDATE tokens SET node_id = ? WHERE hash = ?`, nodeID, hashSecret(secret))
	return err
}

func (s *store) addWorker(id, name, nodeID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO workers (id, name, created, last_seen, node_id) VALUES (?, ?, ?, ?, ?)`, id, name, now, now, nodeID)
	return err
}

// setWorkerStopped records an elastic worker stopped (at set) or started (zero).
func (s *store) setWorkerStopped(id string, at time.Time) error {
	var v any
	if !at.IsZero() {
		v = at
	}
	_, err := s.db.Exec(`UPDATE workers SET stopped_at = ? WHERE id = ?`, v, id)
	return err
}

func (s *store) setDraining(id string, on bool) error {
	res, err := s.db.Exec(`UPDATE workers SET draining = ? WHERE id = ?`, on, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no worker %s", id)
	}
	return nil
}

func (s *store) setWorkerGone(id string) error {
	_, err := s.db.Exec(`UPDATE workers SET gone = 1 WHERE id = ?`, id)
	return err
}

// workerSeen records a worker's capacity as of its connection, and when it was last seen.
func (s *store) workerSeen(id string, w api.Worker) error {
	_, err := s.db.Exec(`UPDATE workers SET class = ?, version = ?, cpus = ?, mem_mib = ?, arch = ?, disk_bytes = ?, last_seen = ? WHERE id = ?`,
		w.Class, w.Version, w.CPUs, w.MemMiB, w.Arch, w.Disk.Total, time.Now().UTC(), id)
	return err
}

func (s *store) workers() ([]api.Worker, error) {
	rows, err := s.db.Query(`SELECT id, name, class, version, last_seen, cpus, mem_mib, arch, disk_bytes, node_id, stopped_at, gone, draining FROM workers ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []api.Worker
	for rows.Next() {
		var w api.Worker
		var stopped sql.NullTime
		if err := rows.Scan(&w.ID, &w.Name, &w.Class, &w.Version, &w.LastSeen, &w.CPUs, &w.MemMiB, &w.Arch, &w.Disk.Total, &w.Node, &stopped, &w.Gone, &w.Draining); err != nil {
			return nil, err
		}
		w.Stopped = stopped.Time
		ws = append(ws, w)
	}
	return ws, rows.Err()
}

func (s *store) putBox(b api.Box) error {
	_, err := s.db.Exec(`
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
	_, err := s.db.Exec(`INSERT OR IGNORE INTO version_copies (box_id, worker_id) VALUES (?, ?)`, box, worker)
	return err
}

func (s *store) deleteVersionCopy(box, worker string) error {
	_, err := s.db.Exec(`DELETE FROM version_copies WHERE box_id = ? AND worker_id = ?`, box, worker)
	return err
}

// addCosts adds amounts to a day's ledger, keyed by box and category.
func (s *store) addCosts(day string, add map[[2]string]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, usd := range add {
		if usd == 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO costs (day, box_id, category, usd) VALUES (?, ?, ?, ?)
			ON CONFLICT(day, box_id, category) DO UPDATE SET usd = usd + excluded.usd`, day, k[0], k[1], usd); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) costTotal(day string) (float64, error) {
	var total float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(usd), 0) FROM costs WHERE day = ?`, day).Scan(&total)
	return total, err
}

func (s *store) costs(from string) ([]api.CostRow, error) {
	rows, err := s.db.Query(`SELECT day, box_id, category, usd FROM costs WHERE day >= ? ORDER BY day DESC, box_id, category`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.CostRow
	for rows.Next() {
		var r api.CostRow
		if err := rows.Scan(&r.Day, &r.Box, &r.Category, &r.USD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) setBilled(day string, byCategory map[string]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM billed WHERE day = ?`, day); err != nil {
		return err
	}
	for c, usd := range byCategory {
		if _, err := tx.Exec(`INSERT INTO billed (day, category, usd) VALUES (?, ?, ?)`, day, c, usd); err != nil {
			return err
		}
	}
	// A day with no charges still records that it was checked.
	if len(byCategory) == 0 {
		if _, err := tx.Exec(`INSERT INTO billed (day, category, usd) VALUES (?, '', 0)`, day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) billed(from string) ([]api.CostRow, error) {
	rows, err := s.db.Query(`SELECT day, category, usd FROM billed WHERE day >= ? ORDER BY day DESC`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.CostRow
	for rows.Next() {
		var r api.CostRow
		if err := rows.Scan(&r.Day, &r.Category, &r.USD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) state(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM state WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *store) setState(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
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

func (s *store) deleteBox(id string) error {
	if _, err := s.db.Exec(`DELETE FROM version_copies WHERE box_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM boxes WHERE id = ?`, id)
	return err
}

func (s *store) box(id string) (api.Box, error) {
	var b api.Box
	err := s.db.QueryRow(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, ''), COALESCE(g.branch, ''), COALESCE(g.base, '')
		FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id LEFT JOIN box_git g ON g.box_id = boxes.id WHERE id = ?`, id).
		Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind, &b.Branch, &b.Base)
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("no box %q", id)
	}
	return b, err
}

func (s *store) boxes() ([]api.Box, error) {
	rows, err := s.db.Query(`SELECT id, worker_id, state, image, vcpus, mem_mib, generation, class, reach_ip, created,
		COALESCE(m.project_id, ''), COALESCE(m.kind, ''), COALESCE(g.branch, ''), COALESCE(g.base, '')
		FROM boxes LEFT JOIN box_meta m ON m.box_id = boxes.id LEFT JOIN box_git g ON g.box_id = boxes.id ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bs []api.Box
	for rows.Next() {
		var b api.Box
		if err := rows.Scan(&b.ID, &b.Worker, &b.State, &b.Image, &b.VCPUs, &b.MemMiB, &b.Generation, &b.Class, &b.ReachIP, &b.Created, &b.Project, &b.Kind, &b.Branch, &b.Base); err != nil {
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

func (s *store) addThread(t api.Thread, autonomy string) error {
	_, err := s.db.Exec(`INSERT INTO threads (box_id, n, agent, title, created, session_id, autonomy) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Box, t.N, t.Agent, t.Title, t.Created, t.SessionID, autonomy)
	return err
}

// updateThread sets one column of a thread.
func (s *store) updateThread(box string, n int, column string, value any) error {
	res, err := s.db.Exec(`UPDATE threads SET `+column+` = ? WHERE box_id = ? AND n = ?`, value, box, n)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return fmt.Errorf("no thread %s/%d", box, n)
	}
	return nil
}

func (s *store) deleteThread(box string, n int) error {
	_, err := s.db.Exec(`DELETE FROM threads WHERE box_id = ? AND n = ?`, box, n)
	return err
}

func (s *store) thread(box string, n int) (api.Thread, string, error) {
	t := api.Thread{Box: box, N: n}
	var autonomy string
	err := s.db.QueryRow(`SELECT agent, title, created, session_id, pinned, autonomy FROM threads WHERE box_id = ? AND n = ?`, box, n).
		Scan(&t.Agent, &t.Title, &t.Created, &t.SessionID, &t.Pinned, &autonomy)
	if errors.Is(err, sql.ErrNoRows) {
		return t, "", fmt.Errorf("no thread %s/%d", box, n)
	}
	return t, autonomy, err
}

func (s *store) threads() (map[string][]api.Thread, error) {
	rows, err := s.db.Query(`SELECT box_id, n, agent, title, created, session_id, pinned, renamed FROM threads ORDER BY box_id, n`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byBox := map[string][]api.Thread{}
	for rows.Next() {
		var t api.Thread
		if err := rows.Scan(&t.Box, &t.N, &t.Agent, &t.Title, &t.Created, &t.SessionID, &t.Pinned, &t.Renamed); err != nil {
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
	secrets               string // digest of the project secrets a template box was built with
	version               int    // the template version a box was forked from
}

func (s *store) putMeta(box string, m boxMeta) error {
	_, err := s.db.Exec(`INSERT INTO box_meta (box_id, project_id, kind, commit_sha, verified, secrets, version) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(box_id) DO UPDATE SET project_id = excluded.project_id, kind = excluded.kind,
			commit_sha = excluded.commit_sha, verified = excluded.verified, secrets = excluded.secrets, version = excluded.version`,
		box, m.project, m.kind, m.commit, m.verified, m.secrets, m.version)
	return err
}

func (s *store) meta(box string) (boxMeta, error) {
	var m boxMeta
	err := s.db.QueryRow(`SELECT project_id, kind, commit_sha, verified, secrets FROM box_meta WHERE box_id = ?`, box).
		Scan(&m.project, &m.kind, &m.commit, &m.verified, &m.secrets)
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
		return p, fmt.Errorf("no project %s on this server (run hutch init)", id)
	}
	if err == nil && peaks != "" {
		p.Peaks = &api.Peaks{}
		err = json.Unmarshal([]byte(peaks), p.Peaks)
	}
	return p, err
}

func (s *store) setConfirmed(id, settings string) error {
	_, err := s.db.Exec(`UPDATE projects SET confirmed = ? WHERE id = ?`, settings, id)
	return err
}

func (s *store) setPeaks(id string, p api.Peaks) error {
	data, _ := json.Marshal(p)
	_, err := s.db.Exec(`UPDATE projects SET peaks = ? WHERE id = ?`, string(data), id)
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
	_, err := s.db.Exec(`INSERT INTO templates (project_id, version, commit_sha, box_id, vcpus, mem_mib, created, secrets) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Project, t.Version, t.Commit, t.Box, t.VCPUs, t.MemMiB, t.Created, t.Secrets)
	return err
}

// templates lists a project's versions, newest first.
func (s *store) templates(project string) ([]api.Template, error) {
	rows, err := s.db.Query(`SELECT version, commit_sha, box_id, vcpus, mem_mib, created, secrets FROM templates WHERE project_id = ? ORDER BY version DESC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []api.Template
	for rows.Next() {
		t := api.Template{Project: project}
		if err := rows.Scan(&t.Version, &t.Commit, &t.Box, &t.VCPUs, &t.MemMiB, &t.Created, &t.Secrets); err != nil {
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
	_, err := s.db.Exec(`DELETE FROM user_layer WHERE part = ?`, part)
	return err
}

func (s *store) setBoxCredentials(box string, creds []string) error {
	data, _ := json.Marshal(creds)
	_, err := s.db.Exec(`INSERT INTO box_credentials (box_id, credentials) VALUES (?, ?)
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

func (s *store) setBoxGit(box, branch, base string) error {
	_, err := s.db.Exec(`INSERT INTO box_git (box_id, branch, base) VALUES (?, ?, ?)
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
	_, err := s.db.Exec(`INSERT INTO project_secrets (project_id, digest, content, updated) VALUES (?, ?, ?, ?)
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
	if _, err := s.db.Exec(`DELETE FROM template_builds WHERE project_id = ? AND version = ?`, project, version); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM templates WHERE project_id = ? AND version = ?`, project, version)
	return err
}

// setTemplateParams keeps how a version was built, to build it again for another class.
func (s *store) setTemplateParams(project string, version int, params string) error {
	_, err := s.db.Exec(`UPDATE templates SET params = ? WHERE project_id = ? AND version = ?`, params, project, version)
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
	_, err := s.db.Exec(`INSERT OR REPLACE INTO template_builds (project_id, version, class, box_id) VALUES (?, ?, ?, ?)`, project, version, class, box)
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
	return tx.Commit()
}
