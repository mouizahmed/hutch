//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

// sep separates the fields of tmux's window lines; prompts and titles may hold "|".
const sep = "|~|"

// savedThreads is the box's agent threads as kept on its disk.
type savedThreads struct {
	Last    int                       `json:"last"`
	Threads []guestproto.ThreadStatus `json:"threads"`
}

var savedMu sync.Mutex
var savedLast []byte

// saveThreads writes the box's agent threads to its disk when they change.
func saveThreads(s savedThreads) {
	for i := range s.Threads {
		t := &s.Threads[i]
		s.Threads[i] = guestproto.ThreadStatus{N: t.N, Agent: t.Agent, Autonomy: t.Autonomy, Prompt: t.Prompt, Name: t.Name, Created: t.Created}
	}
	data, _ := json.Marshal(s)
	savedMu.Lock()
	defer savedMu.Unlock()
	if bytes.Equal(data, savedLast) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(guestproto.ThreadsPath), 0o755); err != nil {
		return
	}
	tmp := guestproto.ThreadsPath + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil && os.Rename(tmp, guestproto.ThreadsPath) == nil {
		savedLast = data
	}
}

// restoreThreads recreates the saved agent threads after a cold boot, when the box
// starts with no tmux: each window as its agent exited, one key from its picker.
func restoreThreads() {
	data, err := os.ReadFile(guestproto.ThreadsPath)
	if err != nil {
		return
	}
	var s savedThreads
	if json.Unmarshal(data, &s) != nil || len(s.Threads) == 0 {
		return
	}
	if _, err := tmuxAsDev("has-session", "-t", threadSession); err == nil {
		return
	}
	for i, t := range s.Threads {
		target := threadSession + ":" + strconv.Itoa(t.N)
		var err error
		if i == 0 {
			var first string
			first, err = tmuxAsDev("new-session", "-d", "-s", threadSession, "-x", "200", "-y", "50", "-n", t.Agent, "-c", "/workspace",
				"-P", "-F", "#{window_index}", "--", "true", ";", "set-option", "-w", "remain-on-exit", "on")
			if err == nil && strings.TrimSpace(first) != strconv.Itoa(t.N) {
				_, err = tmuxAsDev("move-window", "-s", threadSession+":"+strings.TrimSpace(first), "-t", target)
			}
		} else {
			_, err = tmuxAsDev("new-window", "-d", "-t", target, "-n", t.Agent, "-c", "/workspace", "--", "true",
				";", "set-option", "-w", "-t", target, "remain-on-exit", "on")
		}
		if err != nil {
			log.Printf("restore thread %d: %v", t.N, err)
			continue
		}
		for opt, v := range map[string]string{guestproto.OptAgent: t.Agent, guestproto.OptAutonomy: t.Autonomy,
			guestproto.OptPrompt: t.Prompt, guestproto.OptName: t.Name, guestproto.OptCreated: strconv.FormatInt(t.Created, 10)} {
			if v != "" {
				tmuxAsDev("set-option", "-w", "-t", target, opt, v)
			}
		}
	}
	tmuxAsDev("set-option", "-t", threadSession, guestproto.OptLast, strconv.Itoa(s.Last))
	savedMu.Lock()
	savedLast = data
	savedMu.Unlock()
}
