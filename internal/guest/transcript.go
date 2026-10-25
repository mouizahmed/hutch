//go:build linux

package guest

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// transcript is what has been read of one session file so far.
type transcript struct {
	path      string
	offset    int64
	title     string
	tokens    int64
	hint      string
	sessionID string
	seen      map[string]bool // Claude message ids already counted
}

var (
	transcriptMu sync.Mutex
	transcripts  = map[int]*transcript{} // by thread number
)

// threadTranscripts parses new transcript lines for every registered agent thread.
func threadTranscripts() map[int]*transcript {
	entries, err := os.ReadDir(guestproto.ThreadsDir)
	if err != nil {
		return nil
	}
	home := "/home/dev"
	if u, err := user.Lookup("dev"); err == nil {
		home = u.HomeDir
	}
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	claimed := map[string]bool{}
	for _, t := range transcripts {
		claimed[t.path] = true
	}
	out := map[int]*transcript{}
	for _, e := range entries {
		file := filepath.Join(guestproto.ThreadsDir, e.Name())
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var info guestproto.ThreadInfo
		if json.Unmarshal(data, &info) != nil {
			continue
		}
		t := transcripts[info.N]
		if t == nil {
			path := findSession(home, info, claimed)
			if path == "" {
				continue
			}
			claimed[path] = true
			t = &transcript{path: path, seen: map[string]bool{}}
			transcripts[info.N] = t
		}
		t.read(info.Parser)
		if info.SessionID == "" && t.sessionID != "" {
			info.SessionID = t.sessionID
			if b, err := json.Marshal(info); err == nil {
				os.WriteFile(file, b, 0o644)
			}
		}
		out[info.N] = t
	}
	// A removed thread's number may be reused.
	for n := range transcripts {
		if out[n] == nil {
			delete(transcripts, n)
		}
	}
	return out
}

// findSession finds a thread's session file: by its session id when the host chose
// one, else the earliest-written unclaimed file under the glob since the thread
// started (agents may open more sessions later, for subagents).
func findSession(home string, info guestproto.ThreadInfo, claimed map[string]bool) string {
	pattern := strings.TrimPrefix(info.Sessions, "~/")
	root := pattern
	if i := strings.IndexAny(root, "*?["); i >= 0 {
		root = filepath.Dir(root[:i])
	}
	base := filepath.Base(pattern)
	var best string
	var bestTime time.Time
	filepath.WalkDir(filepath.Join(home, root), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if ok, _ := filepath.Match(base, d.Name()); !ok || claimed[p] {
			return nil
		}
		if info.SessionID != "" {
			if strings.Contains(d.Name(), info.SessionID) {
				best = p
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().Unix() < info.Started-5 {
			return nil
		}
		if best == "" || fi.ModTime().Before(bestTime) {
			best, bestTime = p, fi.ModTime()
		}
		return nil
	})
	return best
}

// read parses complete lines added since the last read.
func (t *transcript) read(parser string) {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // an incomplete last line is read again next time
		}
		t.offset += int64(len(line))
		switch parser {
		case "claude":
			t.claude(line)
		case "codex":
			t.codex(line)
		}
	}
}

func (t *transcript) claude(line []byte) {
	var e struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		AITitle   string `json:"aiTitle"`
		Message   struct {
			ID         string `json:"id"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				Input         int64 `json:"input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				Output        int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if e.SessionID != "" {
		t.sessionID = e.SessionID
	}
	switch e.Type {
	case "ai-title":
		if e.AITitle != "" {
			t.title = e.AITitle
		}
	case "user":
		t.hint = "working"
	case "assistant":
		u := e.Message.Usage
		if e.Message.ID != "" && !t.seen[e.Message.ID] {
			t.seen[e.Message.ID] = true
			t.tokens += u.Input + u.CacheCreation + u.CacheRead + u.Output
		}
		switch e.Message.StopReason {
		case "end_turn":
			t.hint = "waiting"
		case "tool_use":
			t.hint = "working"
		}
	}
}

func (t *transcript) codex(line []byte) {
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Info struct {
				Total struct {
					Tokens int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch {
	case e.Type == "session_meta" && e.Payload.ID != "":
		t.sessionID = e.Payload.ID
	case e.Payload.Type == "token_count" && e.Payload.Info.Total.Tokens > 0:
		t.tokens = e.Payload.Info.Total.Tokens
	case e.Payload.Type == "task_started":
		t.hint = "working"
	case e.Payload.Type == "task_complete":
		t.hint = "waiting"
	}
}
