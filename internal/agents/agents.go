// Package agents loads adapters: declarative files that tell Hutch how to install,
// start, resume, and log in to an agent, and which of its files are config and
// which are credentials. Built-in adapters use the same format as users' own.
package agents

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type Adapter struct {
	Name        string              `toml:"name"`
	Install     string              `toml:"install"` // shell, as dev, at template build and in login boxes
	Start       string              `toml:"start"`   // command; {prompt} and {session_id} are filled in
	Autonomy    map[string][]string `toml:"autonomy"`
	Resume      string              `toml:"resume"`
	Login       string              `toml:"login"`
	Sessions    string              `toml:"sessions"`
	Credentials []Source            `toml:"credentials"`
	Config      []Source            `toml:"config"`
	MCP         *MCP                `toml:"mcp"`
	Transcript  string              `toml:"transcript"`
	Seed        []Seed              `toml:"seed"`
}

// Source is a file or directory under ~, or with Keys only those top-level keys of
// a JSON file.
type Source struct {
	File string   `toml:"file"`
	Keys []string `toml:"keys"`
}

func (s *Source) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		s.File = v
	case map[string]any:
		f, _ := v["file"].(string)
		s.File = f
		keys, _ := v["keys"].([]any)
		for _, k := range keys {
			ks, ok := k.(string)
			if !ok {
				return errors.New("keys must be strings")
			}
			s.Keys = append(s.Keys, ks)
		}
		for k := range v {
			if k != "file" && k != "keys" {
				return fmt.Errorf("unknown key %q", k)
			}
		}
	default:
		return fmt.Errorf("want a path or { file, keys }, got %v", v)
	}
	if !strings.HasPrefix(s.File, "~/") {
		return fmt.Errorf("%q: paths must start with ~/", s.File)
	}
	return nil
}

// Dir reports whether the source is a whole directory.
func (s Source) Dir() bool { return strings.HasSuffix(s.File, "/") }

type MCP struct {
	File   string `toml:"file"`
	Key    string `toml:"key"`
	Format string `toml:"format"`
}

// Seed is merged into a box's file after the user layer, to get past first-run prompts.
type Seed struct {
	File  string         `toml:"file"`
	Merge map[string]any `toml:"merge"`
}

// Agent reports whether the adapter starts threads; some only log in (gh).
func (a *Adapter) Agent() bool { return a.Start != "" }

//go:embed builtin/*.toml
var builtinFS embed.FS

var builtins = func() map[string]*Adapter {
	m := map[string]*Adapter{}
	entries, _ := builtinFS.ReadDir("builtin")
	for _, e := range entries {
		data, _ := builtinFS.ReadFile(path.Join("builtin", e.Name()))
		a, err := Parse(e.Name(), data)
		if err != nil {
			panic(err)
		}
		m[a.Name] = a
	}
	return m
}()

// Parse reads an adapter file; unknown keys are errors.
func Parse(source string, data []byte) (*Adapter, error) {
	var a Adapter
	md, err := toml.Decode(string(data), &a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	var unknown []string
	for _, k := range md.Undecoded() {
		// Seed contents are free-form.
		if len(k) > 2 && k[0] == "seed" && k[1] == "merge" {
			continue
		}
		unknown = append(unknown, k.String())
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%s: unknown keys: %s", source, strings.Join(unknown, ", "))
	}
	if a.Name == "" {
		return nil, fmt.Errorf("%s: name is required", source)
	}
	if a.Start == "" && a.Login == "" {
		return nil, fmt.Errorf("%s: needs start (an agent) or login", source)
	}
	for mode := range a.Autonomy {
		if mode != "full" && mode != "ask" {
			return nil, fmt.Errorf("%s: autonomy modes are full and ask, not %q", source, mode)
		}
	}
	return &a, nil
}

// Set is the adapters in use: the built-ins, overridden by the user's own.
type Set map[string]*Adapter

// Load combines the built-ins with user adapter files (name → contents).
func Load(user map[string]string) (Set, error) {
	s := Set{}
	for n, a := range builtins {
		s[n] = a
	}
	for name, data := range user {
		a, err := Parse(name, []byte(data))
		if err != nil {
			return nil, err
		}
		s[a.Name] = a
	}
	return s, nil
}

// Builtin is the set without user adapters.
func Builtin() Set {
	s, _ := Load(nil)
	return s
}

// Names lists the adapters that start threads.
func (s Set) Names() []string {
	var names []string
	for n, a := range s {
		if a.Agent() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// Sorted lists every adapter by name.
func (s Set) Sorted() []*Adapter {
	var out []*Adapter
	for _, a := range s {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Argv returns a thread's command: the agent with its autonomy flags and prompt, or
// a generic command run by bash when cmd is set. sessionID fills {session_id} for
// agents that take one at start (see ChoosesSession).
func (s Set) Argv(agent, cmd, prompt, autonomy, sessionID string) ([]string, error) {
	if cmd != "" {
		return []string{"bash", "-lc", cmd}, nil
	}
	a, err := s.agent(agent)
	if err != nil {
		return nil, err
	}
	return fill(a.Start, a.Autonomy[autonomy], prompt, sessionID), nil
}

// ResumeArgv continues a session after the agent's process is gone.
func (s Set) ResumeArgv(agent, autonomy, sessionID string) ([]string, error) {
	a, err := s.agent(agent)
	if err != nil {
		return nil, err
	}
	if a.Resume == "" {
		return nil, fmt.Errorf("the %s adapter has no resume command", agent)
	}
	return fill(a.Resume, a.Autonomy[autonomy], "", sessionID), nil
}

// ChoosesSession reports whether Hutch picks the session id when starting the agent.
func (a *Adapter) ChoosesSession() bool { return strings.Contains(a.Start, "{session_id}") }

func (s Set) agent(name string) (*Adapter, error) {
	a, ok := s[name]
	if !ok || !a.Agent() {
		return nil, fmt.Errorf("unknown agent %q (known: %s; or use --cmd)", name, strings.Join(s.Names(), ", "))
	}
	return a, nil
}

// fill splits a command template into argv. Autonomy flags go where {prompt} is,
// before the prompt, or at the end.
func fill(template string, autonomy []string, prompt, sessionID string) []string {
	var argv []string
	placed := false
	for _, f := range strings.Fields(template) {
		switch f {
		case "{session_id}":
			argv = append(argv, sessionID)
		case "{prompt}":
			argv = append(argv, autonomy...)
			placed = true
			if prompt != "" {
				argv = append(argv, prompt)
			}
		default:
			argv = append(argv, f)
		}
	}
	if !placed {
		argv = append(argv, autonomy...)
	}
	return argv
}

// JSONKeys returns a JSON object of only the given top-level keys of data, or nil
// if none of them are present. The output is canonical (sorted, compact), so the
// same values always give the same bytes however the file was formatted.
func JSONKeys(data []byte, keys []string) ([]byte, error) {
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
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
