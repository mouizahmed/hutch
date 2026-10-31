package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// request resolves a key that a project may only request: the project gets what it
// asks for up to the user's own value plus what the user granted it.
func (c *Config) request(name string, k *Key) Setting {
	base := c.plain(name, k, User)
	pd := c.docs[Project]
	if pd == nil {
		return base
	}
	req, ok := pd.values[name]
	if !ok {
		return base
	}
	var grant any
	if c.ProjectID != "" && c.docs[User] != nil {
		if g, ok := c.docs[User].values[FormatPath(append([]string{"grants", c.ProjectID}, req.path...))]; ok {
			grant = g.parsed
		}
	}
	s := Setting{Path: name, Key: k, Layer: Project, Source: pd.Source, Requested: req.raw}
	switch k.Kind {
	case Enum:
		allowed := slices.Index(k.Enum, asString(base.Value))
		if grant != nil {
			allowed = max(allowed, slices.Index(k.Enum, grant.(string)))
		}
		if slices.Index(k.Enum, req.parsed.(string)) <= allowed {
			s.Raw, s.Value = req.raw, req.parsed
		} else {
			s.Raw, s.Value, s.Withheld = k.Enum[allowed], k.Enum[allowed], req.raw
		}
	case List:
		allowed := asList(base.Value)
		if grant != nil {
			allowed = append(slices.Clone(allowed), grant.([]string)...)
		}
		got, withheld := intersect(req.parsed.([]string), allowed)
		s.Raw, s.Value = anyList(got), got
		if len(withheld) > 0 {
			s.Withheld = anyList(withheld)
		}
	}
	return s
}

// intersect keeps the requested items that are allowed; "*" means every item.
func intersect(req, allowed []string) (got, withheld []string) {
	if slices.Contains(allowed, "*") {
		return req, nil
	}
	got = []string{}
	for _, r := range req {
		switch {
		case r == "*":
			for _, a := range allowed {
				if !slices.Contains(got, a) {
					got = append(got, a)
				}
			}
			withheld = append(withheld, r)
		case slices.Contains(allowed, r):
			if !slices.Contains(got, r) {
				got = append(got, r)
			}
		default:
			withheld = append(withheld, r)
		}
	}
	return got, withheld
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asList(v any) []string {
	l, _ := v.([]string)
	return l
}

func anyList(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// ErrAllowlist refuses a box whose egress would be allowlist: that mode isn't
// built yet, and a box must not start with open egress instead. A stored value
// still loads, so the rest of the config keeps working.
var ErrAllowlist = errors.New(`network.egress = "allowlist" is not built yet; refusing rather than starting with open egress: set it to "open"`)

// Unsupported reports settings boxes can't start with.
func (c *Config) Unsupported() error {
	if c.String("network.egress") == "allowlist" {
		return ErrAllowlist
	}
	return nil
}

// Withheld lists the project's requests that its grants don't cover.
func (c *Config) Withheld() []Setting {
	var out []Setting
	for _, k := range Keys {
		if k.Request {
			if s := c.request(k.Path, k); s.Withheld != nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// Grant adds grants covering the given withheld requests to the user config text.
func (c *Config) Grant(userText []byte, withheld []Setting) ([]byte, error) {
	if c.ProjectID == "" {
		return nil, errors.New("no project to grant to")
	}
	for _, s := range withheld {
		path := append([]string{"grants", c.ProjectID}, strings.Split(s.Key.Path, ".")...)
		raw := s.Requested
		if s.Key.Kind == List {
			merged := anyList(asList(c.grantValue(path)))
			for _, r := range raw.([]any) {
				if !slices.Contains(merged, r) {
					merged = append(merged, r)
				}
			}
			raw = merged
		}
		var err error
		if userText, err = Set(userText, path, raw); err != nil {
			return nil, err
		}
	}
	return userText, nil
}

func (c *Config) grantValue(path []string) any {
	if d := c.docs[User]; d != nil {
		if v, ok := d.values[FormatPath(path)]; ok {
			return v.parsed
		}
	}
	return nil
}

// Repo is the project repository a command runs in.
type Repo struct {
	Root string
	// ID names the project in grants: its origin remote without scheme, user, or
	// ".git", like "github.com/me/my-app", or "local:<root>" without a remote.
	ID string
}

func (r Repo) ConfigPath() string { return filepath.Join(r.Root, ".hutch", "hutch.toml") }

// FindRepo walks up from dir to the root of a Git repository; nil if there is none.
func FindRepo(dir string) (*Repo, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return &Repo{Root: d, ID: projectID(d)}, nil
		}
		if filepath.Dir(d) == d {
			return nil, nil
		}
	}
}

func projectID(root string) string {
	out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if url := strings.TrimSpace(string(out)); err == nil && url != "" {
		return NormalizeRemote(url)
	}
	return "local:" + root
}

// NormalizeRemote turns any form of a Git remote URL into host/path.
func NormalizeRemote(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	} else if i := strings.Index(url, ":"); i >= 0 && !strings.Contains(url[:i], "/") {
		url = url[:i] + "/" + url[i+1:] // scp-like: git@host:me/repo
	}
	if i := strings.Index(url, "@"); i >= 0 && i < strings.Index(url+"/", "/") {
		url = url[i+1:]
	}
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if host, rest, ok := strings.Cut(url, "/"); ok {
		return strings.ToLower(host) + "/" + rest
	}
	return strings.ToLower(url)
}
