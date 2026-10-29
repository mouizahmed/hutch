//go:build linux

package guest

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

var (
	sessionMu    sync.Mutex
	sessionFiles = map[int]string{} // by thread number
)

// uuid finds a session id in a session file's name.
var uuid = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// threadSessions finds each registered agent thread's session id: the one the host
// chose, else the last UUID in the name of the session file the agent started.
func threadSessions() map[int]string {
	entries, err := os.ReadDir(guestproto.ThreadsDir)
	if err != nil {
		return nil
	}
	home := "/home/dev"
	if u, err := user.Lookup("dev"); err == nil {
		home = u.HomeDir
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	claimed := map[string]bool{}
	for _, path := range sessionFiles {
		claimed[path] = true
	}
	out := map[int]string{}
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
		if info.SessionID == "" {
			path := sessionFiles[info.N]
			if path == "" {
				path = findSession(home, info, claimed)
			}
			if path == "" {
				continue
			}
			claimed[path] = true
			sessionFiles[info.N] = path
			ids := uuid.FindAllString(filepath.Base(path), -1)
			if len(ids) == 0 {
				continue
			}
			info.SessionID = ids[len(ids)-1]
			if b, err := json.Marshal(info); err == nil {
				os.WriteFile(file, b, 0o644)
			}
		}
		out[info.N] = info.SessionID
	}
	// A removed thread's number may be reused.
	for n := range sessionFiles {
		if _, ok := out[n]; !ok {
			delete(sessionFiles, n)
		}
	}
	return out
}

// findSession finds a thread's session file: the earliest-written unclaimed file
// under the glob since the thread started (agents may open more sessions later, for
// subagents).
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
