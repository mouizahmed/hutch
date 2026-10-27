// Package config resolves settings from four layers (server, user, project, flags)
// over built-in defaults, all checked against one schema.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const Version = 1

// UserPath is the user config, ~/.config/hutch/config.toml: a working copy of the
// server's copy, which is authoritative.
func UserPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "hutch", "config.toml")
}

// Doc is one layer's file, checked against the schema.
type Doc struct {
	Layer  Layer
	Source string
	Text   []byte
	values map[string]value
}

type value struct {
	path   []string
	key    *Key
	raw    any
	parsed any
}

// Parse checks a layer's file: unknown keys, keys that belong to another layer, and
// wrong types or ranges are all errors.
func Parse(layer Layer, source string, text []byte) (*Doc, error) {
	d := &Doc{Layer: layer, Source: source, Text: text, values: map[string]value{}}
	var m map[string]any
	if _, err := toml.Decode(string(text), &m); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if v, ok := m["version"]; ok {
		if n, ok := v.(int64); !ok || n != Version {
			return nil, fmt.Errorf("%s: version %v is not supported by this hutch (want %d)", source, v, Version)
		}
		delete(m, "version")
	}
	var errs []string
	flatten(nil, m, func(path []string, raw any) {
		name := FormatPath(path)
		k := Lookup(path)
		switch {
		case k == nil:
			errs = append(errs, fmt.Sprintf("unknown key %s", name))
		case !k.allowed(layer):
			errs = append(errs, fmt.Sprintf("%s can't be set in the %s config (it belongs in: %s)", name, layer, layerList(k.Layers)))
		default:
			p, err := k.Parse(raw)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
				return
			}
			d.values[name] = value{path: path, key: k, raw: raw, parsed: p}
		}
	})
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("%s:\n  %s", source, strings.Join(errs, "\n  "))
	}
	return d, nil
}

// ReadFile parses a layer's file; a missing file is an empty layer.
func ReadFile(layer Layer, path string) (*Doc, error) {
	text, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return Parse(layer, path, text)
}

func flatten(prefix []string, m map[string]any, leaf func([]string, any)) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := append(slices.Clone(prefix), k)
		if sub, ok := m[k].(map[string]any); ok {
			flatten(path, sub, leaf)
		} else {
			leaf(path, m[k])
		}
	}
}

func layerList(ls []Layer) string {
	s := make([]string, len(ls))
	for i, l := range ls {
		s[i] = l.String()
	}
	return strings.Join(s, ", ")
}

var bare = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// FormatPath writes a path as a TOML dotted key, quoting segments that need it.
func FormatPath(path []string) string {
	s := make([]string, len(path))
	for i, p := range path {
		if bare.MatchString(p) {
			s[i] = p
		} else {
			s[i] = quote(p)
		}
	}
	return strings.Join(s, ".")
}

// SplitPath reads a dotted key, with "..." quoting for segments that contain dots.
func SplitPath(s string) ([]string, error) {
	var path []string
	for {
		s = strings.TrimSpace(s)
		var seg string
		if strings.HasPrefix(s, `"`) {
			end := strings.Index(s[1:], `"`)
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote in key")
			}
			seg, s = s[1:end+1], strings.TrimSpace(s[end+2:])
		} else {
			i := strings.IndexAny(s, ". \t=")
			if i < 0 {
				i = len(s)
			}
			seg, s = s[:i], strings.TrimSpace(s[i:])
			if !bare.MatchString(seg) {
				return nil, fmt.Errorf("invalid key %q", seg)
			}
		}
		path = append(path, seg)
		if s == "" {
			return path, nil
		}
		if s[0] != '.' {
			return nil, fmt.Errorf("invalid key near %q", s)
		}
		s = s[1:]
	}
}

// Config is the effective configuration for one project (or none).
type Config struct {
	docs [Flag + 1]*Doc
	// ProjectID names the project in grants, like "github.com/me/my-app".
	ProjectID string
}

func New(docs ...*Doc) *Config {
	c := &Config{}
	for _, d := range docs {
		if d != nil {
			c.docs[d.Layer] = d
		}
	}
	return c
}

func (c *Config) Doc(l Layer) *Doc { return c.docs[l] }

// SetFlag sets a value from a command-line flag.
func (c *Config) SetFlag(path string, raw any) error {
	p, err := SplitPath(path)
	if err != nil {
		return err
	}
	k := Lookup(p)
	if k == nil {
		return fmt.Errorf("unknown key %s", path)
	}
	parsed, err := k.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if c.docs[Flag] == nil {
		c.docs[Flag] = &Doc{Layer: Flag, Source: "flags", values: map[string]value{}}
	}
	c.docs[Flag].values[FormatPath(p)] = value{path: p, key: k, raw: raw, parsed: parsed}
	return nil
}

// Setting is a key's effective value and where it came from.
type Setting struct {
	Path   string
	Key    *Key
	Raw    any
	Value  any
	Layer  Layer
	Source string
	// For request keys set by a project: what it asked for, and the part of that
	// its grants don't cover.
	Requested, Withheld any
}

func (s Setting) String() string { return Display(s.Raw) }

// Get resolves one key.
func (c *Config) Get(path string) (Setting, error) {
	p, err := SplitPath(path)
	if err != nil {
		return Setting{}, err
	}
	k := Lookup(p)
	if k == nil {
		return Setting{}, fmt.Errorf("unknown key %s", path)
	}
	name := FormatPath(p)
	if k.Request {
		return c.request(name, k), nil
	}
	return c.plain(name, k, Flag), nil
}

// plain is the highest layer at or below top that sets the key, else its default.
func (c *Config) plain(name string, k *Key, top Layer) Setting {
	for l := top; l > Default; l-- {
		if d := c.docs[l]; d != nil {
			if v, ok := d.values[name]; ok {
				return Setting{Path: name, Key: k, Raw: v.raw, Value: v.parsed, Layer: l, Source: d.Source}
			}
		}
	}
	s := Setting{Path: name, Key: k, Raw: k.Default, Layer: Default, Source: "default"}
	if k.Default != nil {
		s.Value, _ = k.Parse(k.Default)
	}
	return s
}

// All resolves every key that has a value, in schema order, wildcard keys included.
func (c *Config) All() []Setting {
	var out []Setting
	for _, k := range Keys {
		var names []string
		if strings.Contains(k.Path, "*") {
			seen := map[string]bool{}
			for _, d := range c.docs {
				if d == nil {
					continue
				}
				for name, v := range d.values {
					if v.key == k && !seen[name] {
						seen[name] = true
						names = append(names, name)
					}
				}
			}
			sort.Strings(names)
		} else {
			names = []string{k.Path}
		}
		for _, name := range names {
			s, _ := c.Get(name)
			if s.Raw != nil {
				out = append(out, s)
			}
		}
	}
	return out
}

func (c *Config) must(path string) any {
	s, err := c.Get(path)
	if err != nil {
		panic(err)
	}
	return s.Value
}

func (c *Config) String(path string) string {
	v, _ := c.must(path).(string)
	return v
}

func (c *Config) Int(path string) int64 { return c.must(path).(int64) }

func (c *Config) Duration(path string) time.Duration { return c.must(path).(time.Duration) }

// Size returns bytes.
func (c *Config) Size(path string) int64 { return c.must(path).(int64) }

func (c *Config) Percent(path string) float64 { return c.must(path).(float64) }

func (c *Config) Float(path string) float64 { return c.must(path).(float64) }

func (c *Config) List(path string) []string {
	v, _ := c.must(path).([]string)
	return v
}
