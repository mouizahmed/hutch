package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Threads live in the box's tmux session "hutch", one window each, numbered from 1.
const tmuxSession = "hutch"

// boxRun runs argv in a box as dev and returns its stdout.
func (s *Server) boxRun(ctx context.Context, w *workerConn, box string, argv ...string) (string, int, error) {
	var stdout, stderr bytes.Buffer
	code, err := rpc.Exec(ctx, w.sess, api.BoxExec, api.ExecParams{ID: box, Argv: argv}, &stdout, &stderr)
	if err != nil {
		return "", code, err
	}
	if code != 0 && stderr.Len() > 0 {
		return stdout.String(), code, fmt.Errorf("%s: %s", argv[0], strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), code, nil
}

// newWindow is how a thread starts: a window at the next unused number in the
// box's "hutch" session, carrying what started in it. Numbers only go up, so a new
// thread never takes a removed one's. An agent's window stays when it exits, for
// its picker; a command's closes, like a terminal tab.
const newWindow = `name=$1 dir=$2 agent=$3 autonomy=$4 prompt=$5 created=$6; shift 6
s=` + tmuxSession + `
last=$(tmux show-options -qv -t $s @hutch-last 2>/dev/null)
saved=$(sed -n 's/.*"last":\([0-9]*\).*/\1/p' ` + guestproto.ThreadsPath + ` 2>/dev/null)
[ "${saved:-0}" -gt "${last:-0}" ] && last=$saved
max=$(tmux list-windows -t $s -F '#{window_index}' 2>/dev/null | sort -n | tail -1)
n=$(( ${last:-0} > ${max:-0} ? ${last:-0} : ${max:-0} )); n=$((n + 1))
keep=off; [ -n "$agent" ] && keep=on
if tmux has-session -t $s 2>/dev/null; then
  tmux new-window -d -t $s:$n -n "$name" -c "$dir" -- "$@" \; set-option -w -t $s:$n remain-on-exit $keep
else
  tmux new-session -d -s $s -x 200 -y 50 -n "$name" -c "$dir" -- "$@" \; set-option -w remain-on-exit $keep \; move-window -t $s:$n
fi
w=$s:$n
tmux set-option -w -t $w @hutch-agent "$agent" \; set-option -w -t $w @hutch-autonomy "$autonomy" \; \
  set-option -w -t $w @hutch-prompt "$prompt" \; set-option -w -t $w @hutch-created "$created" \; set-option -t $s @hutch-last $n
echo $n`

func (s *Server) threadNew(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if p.Agent == "" && p.Cmd == "" {
		p.Agent = "claude"
	}
	if p.Autonomy == "" {
		p.Autonomy = "full"
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	var argv []string
	if p.Resume != "" {
		argv, err = set.ResumeArgv(p.Agent, p.Autonomy, p.Resume)
	} else {
		argv, err = set.Argv(p.Agent, p.Cmd, p.Prompt, p.Autonomy, p.Model)
	}
	if err != nil {
		return nil, err
	}
	b, err := s.store.box(p.Box)
	if err != nil {
		return nil, err
	}
	dir, err := threadDir(b, p.Dir)
	if err != nil {
		return nil, err
	}
	agent, name := p.Agent, p.Agent
	if p.Cmd != "" {
		agent, name = "", "cmd"
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	if err := s.writeFiles(ctx, w, p.Box, p.Files); err != nil {
		return nil, fmt.Errorf("session files: %w", err)
	}
	prompt := agents.Title(p.Agent, p.Cmd, p.Prompt)
	if p.Resume != "" {
		prompt = p.Agent + " session from your laptop"
	}
	created := time.Now().UTC().Truncate(time.Second)
	args := append([]string{"sh", "-c", newWindow, "sh", name, dir, agent, p.Autonomy, prompt, strconv.FormatInt(created.Unix(), 10)}, argv...)
	out, code, err := s.boxRun(ctx, w, p.Box, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("tmux exited %d", code)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return nil, fmt.Errorf("tmux window index %q: %w", out, err)
	}
	s.store.event(p.Box, api.ThreadNew, fmt.Sprintf("%d %s", n, prompt))
	return api.Thread{Box: p.Box, N: n, Agent: agent, Autonomy: p.Autonomy, Title: prompt, Created: created}, nil
}

// thread is a box's thread as it last reported it.
func (s *Server) thread(box string, n int) (api.Box, api.Thread, error) {
	b, err := s.store.box(box)
	if err != nil {
		return b, api.Thread{}, err
	}
	for _, t := range b.Threads {
		if t.N == n {
			return b, t, nil
		}
	}
	return b, api.Thread{}, fmt.Errorf("no thread %s/%d", box, n)
}

// dropThread takes a thread out of its box's cached list at once, before the next
// report confirms it.
func (s *Server) dropThread(b api.Box, n int) {
	var keep []api.Thread
	for _, t := range b.Threads {
		if t.N != n {
			keep = append(keep, t)
		}
	}
	s.store.setThreads(b.ID, keep)
}

func (s *Server) threadRm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	b, _, err := s.thread(p.Box, p.N)
	if err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	s.boxRun(ctx, w, p.Box, "tmux", "kill-window", "-t", fmt.Sprintf("%s:%d", tmuxSession, p.N))
	s.store.event(p.Box, api.ThreadRm, strconv.Itoa(p.N))
	s.dropThread(b, p.N)
	return nil, nil
}

func (s *Server) threadRename(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(p.Title)
	if name == "" {
		return nil, fmt.Errorf("empty name")
	}
	if _, _, err := s.thread(p.Box, p.N); err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	_, code, err := s.boxRun(ctx, w, p.Box, "tmux", "set-option", "-w", "-t", fmt.Sprintf("%s:%d", tmuxSession, p.N), guestproto.OptName, name)
	if err == nil && code != 0 {
		err = fmt.Errorf("tmux exited %d", code)
	}
	return nil, err
}

// threadResume runs the agent's picker in a thread whose agent exited.
func (s *Server) threadResume(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	b, t, err := s.thread(p.Box, p.N)
	if err != nil {
		return nil, err
	}
	if t.Agent == "" {
		return nil, fmt.Errorf("%s/%d runs a command, not an agent", p.Box, p.N)
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	argv, err := set.PickArgv(t.Agent, t.Autonomy)
	if err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s:%d", tmuxSession, p.N)
	if dead, _ := s.paneDead(ctx, w, p.Box, target); !dead {
		return nil, fmt.Errorf("%s/%d's %s is still running", p.Box, p.N, t.Agent)
	}
	dir, _ := threadDir(b, "")
	cmd := append([]string{"tmux", "respawn-pane", "-t", target, "-c", dir, "--"}, argv...)
	if _, code, err := s.boxRun(ctx, w, p.Box, cmd...); err != nil || code != 0 {
		return nil, fmt.Errorf("respawn: exit %d %v", code, err)
	}
	s.store.event(p.Box, api.ThreadResume, strconv.Itoa(p.N))
	return t, nil
}

func (s *Server) paneDead(ctx context.Context, w *workerConn, box, target string) (bool, error) {
	out, _, err := s.boxRun(ctx, w, box, "tmux", "display-message", "-p", "-t", target, "#{pane_dead}")
	return strings.TrimSpace(out) == "1", err
}

func (s *Server) threadSend(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.SendParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s:%d", tmuxSession, p.N)
	if dead, err := s.paneDead(ctx, w, p.Box, target); err != nil {
		return nil, err
	} else if dead {
		return nil, fmt.Errorf("the agent in %s/%d exited; open it to resume (hutch attach %s/%d)", p.Box, p.N, p.Box, p.N)
	}
	// A bracketed paste, then Enter a moment later: typed fast, a long message reads
	// as a paste to agents, which take an Enter inside it as a newline.
	_, _, err = s.boxRun(ctx, w, p.Box, "sh", "-c", `tmux set-buffer -b hutch-send -- "$2" && tmux paste-buffer -p -d -b hutch-send -t "$1" && sleep 0.3 && tmux send-keys -t "$1" Enter`, "sh", target, p.Message)
	return nil, err
}
