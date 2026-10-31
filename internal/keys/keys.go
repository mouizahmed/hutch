// Package keys keeps agent and GitHub logins out of boxes. Boxes hold
// placeholders; the proxy swaps in the real values on the way out, toward the hosts
// each login is for, and handles the agents' token refreshes itself.
package keys

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Port is where the proxy listens in each box's network, at its gateway address.
// It's part of what a box's snapshot remembers, so it doesn't change.
const Port = 3128

// Gateway is a box's gateway address, the same in every box.
const Gateway = "169.254.0.21"

// CAPath is where a box keeps the machine's CA, which signs the proxy's certificates.
const CAPath = "/usr/local/share/ca-certificates/hutch.crt"

// Table is everything the proxy needs, from the server.
type Table struct {
	Logins []Login `json:"logins"`
	// Grants lists, per box, the logins it may use; "*" means all of them.
	Grants map[string][]string `json:"grants"`
	CACert []byte              `json:"ca_cert"`
	CAKey  []byte              `json:"ca_key"`
	// Reuse: a refresh this soon after the last one is answered from it.
	Reuse time.Duration `json:"reuse"`
	// RefreshTimeout bounds a refresh upstream and storing its result, which
	// carry on when the box stops waiting.
	RefreshTimeout time.Duration `json:"refresh_timeout"`
	// Identity is opt-in by exact host; the seed stays on the machine.
	IdentitySeed  []byte            `json:"identity_seed,omitempty"`
	IdentityHosts []string          `json:"identity_hosts,omitempty"`
	Projects      map[string]string `json:"projects,omitempty"` // box → project
}

// Login is a set of secrets and the hosts they're for: an adapter's login (Name
// is the adapter's), or one of its MCP servers' keys (Name is
// "<adapter>/mcp/<server>"). A box may use it if it's granted Adapter's logins.
type Login struct {
	Name    string   `json:"name"`
	Adapter string   `json:"adapter"`
	Hosts   []string `json:"hosts"`
	Secrets []Secret `json:"secrets"`
	// TokenURL is where the login refreshes its tokens (OAuth 2); empty for one
	// that doesn't.
	TokenURL string `json:"token_url,omitempty"`
	// Headers are the request headers its placeholders are swapped in, besides
	// AuthHeaders: an MCP server's own key headers.
	Headers []string `json:"headers,omitempty"`
}

// AuthHeaders are the headers every login's placeholders are swapped in.
var AuthHeaders = []string{"Authorization", "X-Api-Key"}

// Refreshes reports whether the login refreshes its tokens through the proxy.
func (l Login) Refreshes() bool { return l.TokenURL != "" }

// refreshSecret is the login's refresh token, if it has one.
func (l Login) refreshSecret() (Secret, bool) {
	for _, s := range l.Secrets {
		if s.Key == Norm("refresh_token") {
			return s, true
		}
	}
	return Secret{}, false
}

// ApplyChanges updates secrets with a refresh's changes. Every placeholder a
// secret had keeps standing for it, so copies boxes hold stay valid.
func ApplyChanges(secrets []Secret, changes []Change) []Secret {
	secrets = slices.Clone(secrets)
	for _, c := range changes {
		i := slices.IndexFunc(secrets, func(x Secret) bool { return x.Key == c.Key && x.Real == c.Old })
		if i < 0 {
			// An acknowledgement can be lost after a successful store. A retry
			// adds any missing alias, without duplicating or reverting a token.
			i = slices.IndexFunc(secrets, func(x Secret) bool { return x.Key == c.Key && x.Real == c.New })
		}
		if i < 0 {
			if slices.ContainsFunc(secrets, func(x Secret) bool { return x.Key == c.Key }) {
				continue // This change belongs to an older login or refresh.
			}
			secrets = append(secrets, Secret{Key: c.Key, Real: c.New, Placeholders: []string{c.Placeholder}})
			continue
		}
		sec := secrets[i]
		sec.Real = c.New
		if !slices.Contains(sec.Placeholders, c.Placeholder) {
			sec.Placeholders = append([]string{c.Placeholder}, sec.Placeholders...)
		}
		secrets[i] = sec
	}
	return secrets
}

// Secret is one value in a login's files. Every placeholder it was ever given
// stands for its current real value; the first is the newest.
type Secret struct {
	Key          string   `json:"key"`
	Real         string   `json:"real"`
	Placeholders []string `json:"placeholders"`
}

// Change is a secret's new real value after a refresh, with the placeholder that
// now stands for it.
type Change struct {
	Key         string `json:"key"`
	Old         string `json:"old"`
	New         string `json:"new"`
	Placeholder string `json:"placeholder"`
}

// Norm is a key name as secrets are matched by it: accessToken, access_token
// and access-token are one key.
func Norm(key string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
}

// ForHost reports whether a login is for host: one of its hosts or a subdomain.
func (l Login) ForHost(host string) bool {
	for _, h := range l.Hosts {
		if matchesHost(host, h) {
			return true
		}
	}
	return false
}

func matchesHost(host, allowed string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	allowed = strings.TrimSuffix(strings.ToLower(allowed), ".")
	return host == allowed || strings.HasSuffix(host, "."+allowed)
}

// ForTarget includes the HTTPS port in a login's boundary. Bare host declarations
// mean 443; a nonstandard port must be explicitly declared as host:port.
func (l Login) ForTarget(host, port string) bool {
	for _, target := range l.Hosts {
		allowed, wantPort, err := net.SplitHostPort(target)
		if err != nil {
			allowed, wantPort = target, "443"
		}
		if port == wantPort && matchesHost(host, allowed) {
			return true
		}
	}
	return false
}

// Granted reports whether grants include adapter.
func Granted(grants []string, adapter string) bool {
	for _, g := range grants {
		if g == "*" || g == adapter {
			return true
		}
	}
	return false
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func random(n int) string {
	b := make([]byte, max(n, 0))
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		b[i] = alnum[k.Int64()]
	}
	return string(b)
}

// Placeholder makes a stand-in for a secret with its shape: a JWT keeps its
// header and claims under a fake signature, since clients read the claims; any
// other value keeps its prefix (sk-ant-oat01-, gho_) and length.
func Placeholder(real string) string {
	if IsJWT(real) {
		p := strings.Split(real, ".")
		return p[0] + "." + p[1] + ".hutch" + random(len(p[2])-5)
	}
	prefix := ""
	if i := strings.LastIndexAny(real[:min(len(real), 16)], "-_"); i >= 0 {
		prefix = real[:i+1]
	}
	return prefix + "hutch" + random(max(len(real)-len(prefix)-5, 16))
}

// IsJWT reports whether a value is a JWT, whose placeholder carries its claims.
func IsJWT(s string) bool {
	p := strings.Split(s, ".")
	if len(p) != 3 || len(p[2]) <= 5 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p[1])
	return err == nil && json.Valid(b)
}

// LooksSecret reports whether a configured value is a key rather than a setting:
// a token of at least 16 characters with no spaces, after an optional auth
// scheme ("Bearer …"), and not a path or URL.
func LooksSecret(v string) bool {
	for _, scheme := range []string{"Bearer ", "Basic ", "Token ", "token ", "bearer "} {
		v = strings.TrimPrefix(v, scheme)
	}
	if len(v) < 16 || strings.ContainsAny(v, " \t\n") || strings.ContainsAny(v[:1], "/~.") || strings.Contains(v, "://") {
		return false
	}
	return true
}

// Find returns the string values under any of keys, anywhere in a JSON or TOML
// file; in other files (gh's YAML), values on "key: value" lines.
func Find(path string, data []byte, keys []string) map[string]string {
	want := map[string]bool{}
	for _, k := range keys {
		want[Norm(k)] = true
	}
	found := map[string]string{}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && s != "" && want[Norm(k)] {
					found[s] = Norm(k)
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		case []map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	var doc any
	switch {
	case strings.HasSuffix(path, ".json") && json.Unmarshal(data, &doc) == nil:
		walk(doc)
	case strings.HasSuffix(path, ".toml"):
		var m map[string]any
		if _, err := toml.Decode(string(data), &m); err == nil {
			walk(m)
		}
	default:
		for _, m := range yamlLine.FindAllSubmatch(data, -1) {
			if want[Norm(string(m[1]))] {
				found[string(m[2])] = Norm(string(m[1]))
			}
		}
	}
	return found
}

var yamlLine = regexp.MustCompile(`(?m)^[ \t-]*([A-Za-z0-9_-]+):[ \t]*["']?([^\s"'#]+)`)

// Replace swaps every old value in data for its new one.
func Replace(data []byte, swaps map[string]string) []byte {
	var old []string
	for value := range swaps {
		if value != "" {
			old = append(old, value)
		}
	}
	sort.Slice(old, func(i, j int) bool {
		if len(old[i]) != len(old[j]) {
			return len(old[i]) > len(old[j])
		}
		return old[i] < old[j]
	})
	var pairs []string
	for _, value := range old {
		pairs = append(pairs, value, swaps[value])
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(data)))
}

// ReplaceFile substitutes decoded string values so escaped JSON/TOML tokens
// cannot escape replacement. Numbers and unrelated metadata retain their types.
func ReplaceFile(path string, data []byte, swaps map[string]string) ([]byte, error) {
	if len(swaps) == 0 {
		return bytes.Clone(data), nil
	}
	var doc any
	switch {
	case strings.HasSuffix(path, ".json"):
		if !json.Valid(data) {
			return nil, fmt.Errorf("invalid JSON credential file")
		}
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if err := d.Decode(&doc); err != nil {
			return nil, fmt.Errorf("invalid JSON credential file")
		}
	case strings.HasSuffix(path, ".toml"):
		var m map[string]any
		if _, err := toml.Decode(string(data), &m); err != nil {
			return nil, fmt.Errorf("invalid TOML credential file")
		}
		doc = m
	default:
		return Replace(data, swaps), nil
	}
	var walk func(any) any
	walk = func(value any) any {
		switch value := value.(type) {
		case string:
			return string(Replace([]byte(value), swaps))
		case map[string]any:
			for key, item := range value {
				value[key] = walk(item)
			}
		case []any:
			for i, item := range value {
				value[i] = walk(item)
			}
		case []map[string]any:
			for _, item := range value {
				walk(item)
			}
		}
		return value
	}
	doc = walk(doc)
	if strings.HasSuffix(path, ".json") {
		return json.Marshal(doc)
	}
	var out bytes.Buffer
	err := toml.NewEncoder(&out).Encode(doc)
	return out.Bytes(), err
}
