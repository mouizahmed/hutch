// Package keys keeps agent and GitHub logins out of boxes. Boxes hold
// placeholders; the proxy swaps in the real values on the way out, toward the hosts
// each login is for, and handles the agents' token refreshes itself.
package keys

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"regexp"
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
}

// Refreshes reports whether the login is an agent's, which refreshes its tokens.
func (l Login) Refreshes() bool { return l.Name == l.Adapter }

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
		if host == h || strings.HasSuffix(host, "."+h) {
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
	for old, nw := range swaps {
		data = bytes.ReplaceAll(data, []byte(old), []byte(nw))
	}
	return data
}
