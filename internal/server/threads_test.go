package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func TestThreadMissingAgent(t *testing.T) {
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var commands []api.ExecParams
	serveWorker(t, s, "worker", api.Worker{Name: "fixture"}, rpc.Mux{
		api.BoxExec: func(_ context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ExecParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			commands = append(commands, p)
			if len(p.Argv) > 2 && strings.Contains(p.Argv[2], "command -v") {
				return rpc.ExitCode(1), nil
			}
			return rpc.ExitCode(0), out.Stdout([]byte("1\n"))
		},
	})
	if err := s.store.putBox(api.Box{ID: "bare", Worker: "worker", State: "running", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(api.ThreadParams{Box: "bare", Agent: "codex", Prompt: "hello"})
	if _, err := s.threadNew(t.Context(), p, nil); err == nil || !strings.Contains(err.Error(), "codex isn't installed in box bare") {
		t.Fatalf("missing agent: %v", err)
	}
	if len(commands) != 1 || commands[0].Argv[len(commands[0].Argv)-1] != "codex" {
		t.Fatalf("missing agent was launched: %+v", commands)
	}
	commands = nil
	p, _ = json.Marshal(api.ThreadParams{Box: "bare", Cmd: "bash"})
	if _, err := s.threadNew(t.Context(), p, nil); err != nil {
		t.Fatal("a shell in a bare box:", err)
	}
	if len(commands) != 1 || !strings.Contains(commands[0].Argv[2], "tmux new-window") {
		t.Fatalf("--cmd was checked as an agent: %+v", commands)
	}
}
