//go:build linux

package guest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

var pushState atomic.Pointer[guestproto.Push]

// pushLoop pushes the box's branch to origin when it has new commits, at most once
// per interval, so committed work leaves the box even while the host is unreachable.
// Pushes are never forced: when origin has moved, the push fails and status says so.
func pushLoop(ctx context.Context) {
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		data, err := os.ReadFile(guestproto.PushConfigPath)
		if err != nil {
			continue
		}
		var conf guestproto.PushConfig
		if json.Unmarshal(data, &conf) != nil || conf.Branch == "" || time.Since(last) < conf.Interval {
			continue
		}
		git := func(args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			out, err := asDev(ctx, conf.Dir, "git", args...)
			return strings.TrimSpace(out), err
		}
		head, err := git("rev-parse", "-q", "--verify", "refs/heads/"+conf.Branch)
		if err != nil || head == "" {
			continue
		}
		if remote, _ := git("rev-parse", "-q", "--verify", "refs/remotes/origin/"+conf.Branch); remote == head {
			continue
		}
		last = time.Now()
		st := &guestproto.Push{Branch: conf.Branch, Head: head, At: last.Unix()}
		if _, err := git("push", "-q", "origin", "refs/heads/"+conf.Branch+":refs/heads/"+conf.Branch); err != nil {
			st.Error = pushError(err.Error())
		}
		pushState.Store(st)
	}
}

func pushError(stderr string) string {
	if strings.Contains(stderr, "[rejected]") || strings.Contains(stderr, "non-fast-forward") {
		return "origin has commits this box doesn't (pushed from elsewhere); pull or rebase in the box"
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	return lines[len(lines)-1]
}
