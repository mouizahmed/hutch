package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
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
	var sessionID string
	var argv []string
	if p.Resume != "" {
		sessionID = p.Resume
		argv, err = set.ResumeArgv(p.Agent, p.Autonomy, p.Resume)
	} else {
		if a := set[p.Agent]; p.Cmd == "" && a != nil && a.ChoosesSession() {
			sessionID = newUUID()
		}
		argv, err = set.Argv(p.Agent, p.Cmd, p.Prompt, p.Autonomy, sessionID)
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
	name := p.Agent
	if p.Cmd != "" {
		name = "cmd"
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	if err := s.writeFiles(ctx, w, p.Box, p.Files); err != nil {
		return nil, fmt.Errorf("session files: %w", err)
	}

	_, code, err := s.boxRun(ctx, w, p.Box, "tmux", "has-session", "-t", tmuxSession)
	if err != nil && code < 0 {
		return nil, err
	}
	var tmux []string
	if code == 0 {
		tmux = []string{"tmux", "new-window", "-t", tmuxSession + ":", "-n", name, "-c", dir, "-P", "-F", "#{window_index}", "--"}
	} else {
		tmux = []string{"tmux", "new-session", "-d", "-s", tmuxSession, "-x", "200", "-y", "50", "-n", name, "-c", dir, "-P", "-F", "#{window_index}", "--"}
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
	title := agents.Title(p.Agent, p.Cmd, p.Prompt)
	if p.Resume != "" {
		title = p.Agent + " session from your laptop"
	}
	t := api.Thread{Box: p.Box, N: n, Agent: name, Title: title, Created: time.Now().UTC(), SessionID: sessionID}
	if a := set[p.Agent]; p.Cmd == "" && a != nil && a.Transcript != "" && a.Transcript != "none" {
		if err := s.registerThread(ctx, w, p.Box, a, t); err != nil {
			return nil, err
		}
	}
	if err := s.store.addThread(t, p.Autonomy); err != nil {
		return nil, err
	}
	s.store.event(p.Box, api.ThreadNew, fmt.Sprintf("%d %s", n, t.Title))
	return t, nil
}

// registerThread tells the guest where the thread's transcript is, so it can report
// the agent's title, tokens, state, and session.
func (s *Server) registerThread(ctx context.Context, w *workerConn, box string, a *agents.Adapter, t api.Thread) error {
	info, _ := json.Marshal(guestproto.ThreadInfo{N: t.N, Parser: a.Transcript, Sessions: a.Sessions, SessionID: t.SessionID, Started: t.Created.Unix()})
	return rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{User: "root",
		Files: []guestproto.File{{Path: fmt.Sprintf("%s/%d.json", guestproto.ThreadsDir, t.N), Mode: 0o644, Data: info}}}}, nil)
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Server) threadRm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, _, err := s.store.thread(p.Box, p.N); err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	s.boxRun(ctx, w, p.Box, "tmux", "kill-window", "-t", fmt.Sprintf("%s:%d", tmuxSession, p.N))
	s.run(ctx, w, p.Box, api.ExecParams{User: "root", Argv: []string{"rm", "-f", fmt.Sprintf("%s/%d.json", guestproto.ThreadsDir, p.N)}}, io.Discard, io.Discard)
	s.store.event(p.Box, api.ThreadRm, strconv.Itoa(p.N))
	return nil, s.store.deleteThread(p.Box, p.N)
}

func (s *Server) threadPin(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	return nil, s.store.updateThread(p.Box, p.N, "pinned", p.Pinned)
}

func (s *Server) threadRename(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("empty title")
	}
	if err := s.store.updateThread(p.Box, p.N, "title", p.Title); err != nil {
		return nil, err
	}
	return nil, s.store.updateThread(p.Box, p.N, "renamed", true)
}

// threadResume restarts an agent whose process has exited (or was lost to a cold
// boot) in its own session, in the same tmux window.
func (s *Server) threadResume(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ThreadTarget
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	t, autonomy, err := s.store.thread(p.Box, p.N)
	if err != nil {
		return nil, err
	}
	if t.SessionID == "" {
		return nil, fmt.Errorf("%s/%d has no known session to resume", p.Box, p.N)
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	argv, err := set.ResumeArgv(t.Agent, autonomy, t.SessionID)
	if err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	target := fmt.Sprintf("%s:%d", tmuxSession, p.N)
	out, _, err := s.boxRun(ctx, w, p.Box, "tmux", "display-message", "-p", "-t", target, "#{pane_dead}")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "0" {
		return nil, fmt.Errorf("%s/%d is still running", p.Box, p.N)
	}
	b, err := s.store.box(p.Box)
	if err != nil {
		return nil, err
	}
	dir, _ := threadDir(b, "")
	cmd := append([]string{"tmux", "respawn-pane", "-t", target, "-c", dir, "--"}, argv...)
	if _, code, err := s.boxRun(ctx, w, p.Box, cmd...); err != nil || code != 0 {
		return nil, fmt.Errorf("respawn: exit %d %v", code, err)
	}
	s.store.event(p.Box, api.ThreadResume, strconv.Itoa(p.N))
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
	// A bracketed paste, then Enter a moment later: typed fast, a long message reads
	// as a paste to agents, which take an Enter inside it as a newline.
	target := fmt.Sprintf("%s:%d", tmuxSession, p.N)
	_, _, err = s.boxRun(ctx, w, p.Box, "sh", "-c", `tmux set-buffer -b hutch-send -- "$2" && tmux paste-buffer -p -d -b hutch-send -t "$1" && sleep 0.3 && tmux send-keys -t "$1" Enter`, "sh", target, p.Message)
	return nil, err
}
