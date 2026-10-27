package agents

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Session is an agent session on this machine: its files, relative to home.
type Session struct {
	Agent    string
	ID       string
	Files    []string
	Modified time.Time
}

// SessionsRoot is the directory, relative to home, that the adapter's sessions glob
// searches.
func (a *Adapter) SessionsRoot() string {
	root := strings.TrimPrefix(a.Sessions, "~/")
	if i := strings.IndexAny(root, "*?["); i >= 0 {
		root = filepath.Dir(root[:i])
	}
	return root
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeKey is the directory Claude Code keeps a working directory's sessions in.
func claudeKey(dir string) string { return nonAlnum.ReplaceAllString(dir, "-") }

// LatestSession returns the adapter's most recently written session that ran in
// dir, or nil. Finding sessions by directory depends on the transcript format.
func LatestSession(a *Adapter, home, dir string) (*Session, error) {
	root := filepath.Join(home, a.SessionsRoot())
	switch a.Transcript {
	case "claude":
		keyDir := filepath.Join(root, claudeKey(dir))
		files, err := filepath.Glob(filepath.Join(keyDir, "*.jsonl"))
		if err != nil || len(files) == 0 {
			return nil, err
		}
		newest, at := newestFile(files)
		id := strings.TrimSuffix(filepath.Base(newest), ".jsonl")
		s := &Session{Agent: a.Name, ID: id, Modified: at}
		rel, _ := filepath.Rel(home, newest)
		s.Files = append(s.Files, rel)
		// Subagent transcripts and large tool results live beside it.
		filepath.WalkDir(filepath.Join(keyDir, id), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				rel, _ := filepath.Rel(home, p)
				s.Files = append(s.Files, rel)
			}
			return nil
		})
		return s, nil
	case "codex":
		var files []string
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
				files = append(files, p)
			}
			return nil
		})
		mtimes := map[string]time.Time{}
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil {
				mtimes[f] = fi.ModTime()
			}
		}
		sort.Slice(files, func(i, j int) bool { return mtimes[files[i]].After(mtimes[files[j]]) })
		for _, f := range files {
			id, cwd, sub := codexMeta(f)
			if id == "" || sub || cwd != dir {
				continue
			}
			rel, _ := filepath.Rel(home, f)
			return &Session{Agent: a.Name, ID: id, Files: []string{rel}, Modified: mtimes[f]}, nil
		}
	}
	return nil, nil
}

func newestFile(files []string) (string, time.Time) {
	var best string
	var at time.Time
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(at) {
			best, at = f, fi.ModTime()
		}
	}
	return best, at
}

// codexMeta reads a Codex session's first line: its id, working directory, and
// whether it is a subagent's.
func codexMeta(path string) (id, cwd string, subagent bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			ID     string `json:"id"`
			Cwd    string `json:"cwd"`
			Parent string `json:"parent_thread_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "session_meta" {
		return
	}
	return e.Payload.ID, e.Payload.Cwd, e.Payload.Parent != ""
}

// Relocate rewrites a session file that ran in from for use in to: its path under
// home (Claude Code keys sessions by directory) and each mention of the directory
// in it, so the history's file paths point at the code there.
func Relocate(transcript, rel string, data []byte, from, to string) (string, []byte) {
	if from == to {
		return rel, data
	}
	if transcript == "claude" {
		rel = strings.Replace(rel, "/"+claudeKey(from)+"/", "/"+claudeKey(to)+"/", 1)
	}
	re := regexp.MustCompile(regexp.QuoteMeta(from) + `([/"\\])`)
	return rel, re.ReplaceAll(data, []byte(strings.ReplaceAll(to, "$", "$$")+"${1}"))
}
