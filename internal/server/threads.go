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

func (s *Server) threadNew(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if p.Agent == "" && p.Cmd == "" {
		p.Agent = "claude"
	}
	argv, err := agents.Argv(p.Agent, p.Cmd, p.Prompt)
	if err != nil {
		return nil, err
	}
	name := p.Agent
	if p.Cmd != "" {
		name = "cmd"
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}

	_, code, err := s.boxRun(ctx, w, p.Box, "tmux", "has-session", "-t", tmuxSession)
	if err != nil && code < 0 {
		return nil, err
	}
	var tmux []string
	if code == 0 {
		tmux = []string{"tmux", "new-window", "-t", tmuxSession + ":", "-n", name, "-c", "/workspace", "-P", "-F", "#{window_index}", "--"}
	} else {
		tmux = []string{"tmux", "new-session", "-d", "-s", tmuxSession, "-x", "200", "-y", "50", "-n", name, "-c", "/workspace", "-P", "-F", "#{window_index}", "--"}
	}
	out, code, err := s.boxRun(ctx, w, p.Box, append(tmux, argv...)...)
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
	t := api.Thread{Box: p.Box, N: n, Agent: name, Title: agents.Title(p.Agent, p.Cmd, p.Prompt), Created: time.Now().UTC()}
	if err := s.store.addThread(t); err != nil {
		return nil, err
	}
	s.store.event(p.Box, api.ThreadNew, fmt.Sprintf("%d %s", n, t.Title))
	return t, nil
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
	if _, _, err := s.boxRun(ctx, w, p.Box, "tmux", "send-keys", "-t", target, "-l", "--", p.Message); err != nil {
		return nil, err
	}
	_, _, err = s.boxRun(ctx, w, p.Box, "tmux", "send-keys", "-t", target, "Enter")
	return nil, err
}
