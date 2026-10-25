// Package agents knows how to start each supported agent with full autonomy.
// Phase 2 replaces this with declarative adapter files.
package agents

import (
	"fmt"
	"sort"
	"strings"
)

var builtin = map[string][]string{
	"claude": {"claude", "--dangerously-skip-permissions"},
	"codex":  {"codex", "--dangerously-bypass-approvals-and-sandbox"},
}

func Names() []string {
	var names []string
	for n := range builtin {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Argv returns the command for a thread: the agent with its prompt, or a generic
// command run by bash when cmd is set.
func Argv(agent, cmd, prompt string) ([]string, error) {
	if cmd != "" {
		return []string{"bash", "-lc", cmd}, nil
	}
	base, ok := builtin[agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (known: %s; or use --cmd)", agent, strings.Join(Names(), ", "))
	}
	argv := append([]string(nil), base...)
	if prompt != "" {
		argv = append(argv, prompt)
	}
	return argv, nil
}

// Title is the thread's name in lists: the first line of the prompt.
func Title(agent, cmd, prompt string) string {
	t := prompt
	if t == "" {
		t = cmd
	}
	if t == "" {
		t = agent
	}
	t, _, _ = strings.Cut(t, "\n")
	if r := []rune(t); len(r) > 60 {
		t = string(r[:59]) + "…"
	}
	return t
}
