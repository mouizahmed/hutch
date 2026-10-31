// Package agents loads adapters: declarative files that tell Hutch how to install,
// start, resume, and log in to an agent, and which of its files are config and
// which are credentials. Built-in adapters use the same format as users' own.
package agents

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type Adapter struct {
	Name     string              `toml:"name"`
	Install  string              `toml:"install"` // shell, as dev, at template build and in login boxes
	Start    string              `toml:"start"`   // command; {prompt} is filled in
	Autonomy map[string][]string `toml:"autonomy"`
	// Model is the arguments that choose a model, with {model}; Models are
	// suggestions to pick from.
	Model  string   `toml:"model"`
	Models []string `toml:"models"`
	// Pick is the agent's own conversation picker, run in a thread whose agent
	// exited; Resume continues one session by id ({session_id}), for hutch cloud.
	Pick        string   `toml:"pick"`
	Resume      string   `toml:"resume"`
	Login       string   `toml:"login"`
	Sessions    string   `toml:"sessions"`
	Credentials []Source `toml:"credentials"`
	// Hosts are where its logins are used (and their subdomains): the only hosts the
	// keys proxy swaps their placeholders for real values toward.
	Hosts []string `toml:"hosts"`
	// TokenURL is where its login refreshes (OAuth 2, grant_type=refresh_token);
	// the keys proxy handles refreshes there, and only there.
	TokenURL   string   `toml:"token_url"`
	Config     []Source `toml:"config"`
	MCP        *MCP     `toml:"mcp"`
	Transcript string   `toml:"transcript"`
	Seed       []Seed   `toml:"seed"`
}

// Source is a file or directory under ~, or with Keys only those top-level keys of
// a JSON file. In credentials, Secrets names the keys whose values are secrets,
// anywhere in the file; boxes get placeholders for them.
type Source struct {
	File    string   `toml:"file"`
	Keys    []string `toml:"keys"`
	Secrets []string `toml:"secrets"`
}

func (m MCP) SecretFields() []string {
	return append(append([]string{}, m.Secrets...), m.RawSecrets...)
}

// MCPField reads a dotted path inside one server's configuration.
func MCPField(server map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		var ok bool
		server, ok = server[part].(map[string]any)
		if !ok {
			return nil, false
		}
	}
	v, ok := server[parts[len(parts)-1]]
	return v, ok
}

func TakeMCPField(server map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		var ok bool
		server, ok = server[part].(map[string]any)
		if !ok {
			return nil, false
		}
	}
	key := parts[len(parts)-1]
	v, ok := server[key]
	if ok && v != nil {
		delete(server, key)
	}
	return v, ok && v != nil
}

func SetMCPField(server map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	for _, part := range parts[:len(parts)-1] {
		next, ok := server[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			server[part] = next
		}
		server = next
	}
	server[parts[len(parts)-1]] = value
}

func MCPSecretValues(server map[string]any, fields []string) map[string]string {
	out := map[string]string{}
	for _, field := range fields {
		value, _ := MCPField(server, field)
		switch value := value.(type) {
		case string:
			if value != "" {
				out[field] = value
			}
		case map[string]any:
			for key, v := range value {
				if s, ok := v.(string); ok && s != "" {
					out[field+"."+key] = s
				}
			}
		}
	}
	return out
}

func (s *Source) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		s.File = v
	case map[string]any:
		f, _ := v["file"].(string)
		s.File = f
		for name, list := range map[string]*[]string{"keys": &s.Keys, "secrets": &s.Secrets} {
			items, _ := v[name].([]any)
			for _, k := range items {
				ks, ok := k.(string)
				if !ok {
					return fmt.Errorf("%s must be strings", name)
				}
				*list = append(*list, ks)
			}
		}
		for k := range v {
			if k != "file" && k != "keys" && k != "secrets" {
				return fmt.Errorf("unknown key %q", k)
			}
		}
	default:
		return fmt.Errorf("want a path or { file, keys, secrets }, got %v", v)
	}
	if !strings.HasPrefix(s.File, "~/") {
		return fmt.Errorf("%q: paths must start with ~/", s.File)
	}
	return nil
}

// Dir reports whether the source is a whole directory.
func (s Source) Dir() bool { return strings.HasSuffix(s.File, "/") }

// MCP is where an agent keeps its MCP servers. Secrets are the fields of a
// server that hold keys; sync moves them into the credentials part.
type MCP struct {
	File    string   `toml:"file"`
	Key     string   `toml:"key"`
	Format  string   `toml:"format"`
	Secrets []string `toml:"secrets"`
	// RawSecrets are credential fields used outside HTTPS headers, such as an
	// OAuth client secret sent in a token request body. They are grant-gated but
	// cannot be replaced by this proxy. Fields may use dotted object paths.
	RawSecrets []string `toml:"raw_secrets"`
	// Scoped is a top-level map of the same file whose entries (one per project)
	// hold their own servers under Key; Hutch doesn't transfer those.
	Scoped string `toml:"scoped"`
	// Project is the file in a repository that declares project servers, with
	// its servers under Key.
	Project string `toml:"project"`
	// Fields name a server's settings the readiness check needs beyond the
	// common command, args, env, and url.
	Fields MCPFields `toml:"fields"`
}

// MCPFields are the names an agent uses for a server's HTTP headers, the
// environment variable holding a bearer token, whether it's enabled, and its
// transport type; empty when it has none.
type MCPFields struct {
	Headers   string `toml:"headers" json:"headers,omitempty"`
	BearerEnv string `toml:"bearer_env" json:"bearer_env,omitempty"`
	Enabled   string `toml:"enabled" json:"enabled,omitempty"`
	Type      string `toml:"type" json:"type,omitempty"`
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
	if a.Model != "" && !strings.Contains(a.Model, "{model}") {
		return nil, fmt.Errorf("%s: model needs {model}, like \"--model {model}\"", source)
	}
	if len(a.Models) > 0 && a.Model == "" {
		return nil, fmt.Errorf("%s: models needs model, the arguments that choose one", source)
	}
	if len(a.Credentials) > 0 {
		secrets := false
		for _, c := range a.Credentials {
			secrets = secrets || len(c.Secrets) > 0
		}
		// Without them, a login's real values would go into boxes as they are.
		if u, err := url.Parse(a.TokenURL); a.TokenURL != "" && (err != nil || u.Scheme != "https" || !slices.ContainsFunc(a.Hosts, func(h string) bool {
			return strings.EqualFold(u.Hostname(), h) || strings.HasSuffix(strings.ToLower(u.Hostname()), "."+strings.ToLower(h))
		})) {
			return nil, fmt.Errorf("%s: token_url must be an https URL on one of its hosts", source)
		}
		if !secrets || len(a.Hosts) == 0 {
			return nil, fmt.Errorf("%s: credentials need secrets (the keys holding tokens, like { file = \"~/.x/auth.json\", secrets = [\"access_token\"] }) and hosts (where they're used)", source)
		}
	}
	if a.MCP != nil && len(a.MCP.Secrets) == 0 && len(a.MCP.RawSecrets) == 0 {
		return nil, fmt.Errorf("%s: mcp needs secrets, the server fields that hold keys, like [\"env\"]", source)
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

// Argv returns a thread's command: the agent with its autonomy flags, model, and
// prompt, or a generic command run by bash when cmd is set.
func (s Set) Argv(agent, cmd, prompt, autonomy, model string) ([]string, error) {
	if cmd != "" {
		return []string{"bash", "-lc", cmd}, nil
	}
	a, err := s.agent(agent)
	if err != nil {
		return nil, err
	}
	flags := a.Autonomy[autonomy]
	if model != "" {
		if a.Model == "" {
			return nil, fmt.Errorf("the %s adapter has no model setting, so a model can't be chosen", agent)
		}
		for _, f := range strings.Fields(a.Model) {
			flags = append(flags, strings.ReplaceAll(f, "{model}", model))
		}
	}
	return fill(a.Start, flags, prompt, ""), nil
}

// PickArgv restarts an agent that exited in its thread: its conversation picker,
// or without one, the agent fresh.
func (s Set) PickArgv(agent, autonomy string) ([]string, error) {
	a, err := s.agent(agent)
	if err != nil {
		return nil, err
	}
	cmd := a.Pick
	if cmd == "" {
		cmd = a.Start
	}
	return fill(cmd, a.Autonomy[autonomy], "", ""), nil
}

// ResumeArgv continues one session by id: hutch cloud's, from the laptop.
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

func (s Set) agent(name string) (*Adapter, error) {
	a, ok := s[name]
	if !ok || !a.Agent() {
		return nil, fmt.Errorf("unknown agent %q (known: %s; or use --cmd)", name, strings.Join(s.Names(), ", "))
	}
	return a, nil
}

// fill splits a command template into argv. Flags go where {prompt} is, before
// the prompt, or at the end.
func fill(template string, flags []string, prompt, sessionID string) []string {
	var argv []string
	placed := false
	for _, f := range strings.Fields(template) {
		switch f {
		case "{session_id}":
			argv = append(argv, sessionID)
		case "{prompt}":
			argv = append(argv, flags...)
			placed = true
			if prompt != "" {
				argv = append(argv, prompt)
			}
		default:
			argv = append(argv, f)
		}
	}
	if !placed {
		argv = append(argv, flags...)
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

// MCPServers reads the servers under key in an MCP config file, by name.
func MCPServers(path string, data []byte, key string) map[string]map[string]any {
	servers, _ := ParseMCPServers(path, data, key)
	return servers
}

// ParseMCPServers also reports malformed config rather than treating it as an
// empty server list, for readiness checks.
func ParseMCPServers(path string, data []byte, key string) (map[string]map[string]any, error) {
	var doc map[string]any
	if strings.HasSuffix(path, ".toml") {
		if _, err := toml.Decode(string(data), &doc); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("MCP config is not an object")
	}
	out := map[string]map[string]any{}
	servers, _ := doc[key].(map[string]any)
	if doc[key] != nil && servers == nil {
		return nil, fmt.Errorf("MCP server collection is not an object")
	}
	for name, v := range servers {
		if m, ok := v.(map[string]any); ok {
			out[name] = m
		} else {
			return nil, fmt.Errorf("MCP server configuration is not an object")
		}
	}
	return out, nil
}
