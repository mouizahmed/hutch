package keys

import (
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"testing"
)

// A placeholder must never be the secret, and must keep what clients read: a
// token's prefix and length, a JWT's claims.
func TestPlaceholder(t *testing.T) {
	tok := "sk-ant-oat01-" + strings.Repeat("a1B2", 24)
	ph := Placeholder(tok)
	if ph == tok || !strings.HasPrefix(ph, "sk-ant-oat01-hutch") || len(ph) != len(tok) {
		t.Errorf("opaque: %q", ph)
	}
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	jwt := "eyJhbGciOiJSUzI1NiJ9." + claims + "." + strings.Repeat("s", 40)
	ph = Placeholder(jwt)
	if p := strings.Split(ph, "."); len(p) != 3 || p[1] != claims || strings.Contains(ph, strings.Repeat("s", 40)) {
		t.Errorf("jwt: %q", ph)
	}
}

func TestFind(t *testing.T) {
	for path, data := range map[string]string{
		"~/.claude/.credentials.json": `{"claudeAiOauth":{"accessToken":"AAA","refreshToken":"RRR","scopes":["x"]}}`,
		"~/.codex/config.toml":        "[tokens]\naccess_token = \"AAA\"\nrefresh_token = \"RRR\"\n",
		"~/.config/gh/hosts.yml":      "github.com:\n    users:\n        me:\n            access_token: AAA\n    refresh_token: RRR\n    user: me\n",
	} {
		got := Find(path, []byte(data), []string{"access_token", "refreshToken"})
		if got["AAA"] != "accesstoken" || got["RRR"] != "refreshtoken" || len(got) != 2 {
			t.Errorf("%s: %v", path, got)
		}
	}
}

// Placeholders become real values only for the logins given, including inside
// git's Basic auth.
func TestSwapHeaders(t *testing.T) {
	gh := Login{Name: "gh", Adapter: "gh", Secrets: []Secret{{Key: "oauthtoken", Real: "gho_REAL", Placeholders: []string{"gho_PH"}}}}
	h := http.Header{
		"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:gho_PH"))},
		"X-Other":       {"token gho_PH"},
	}
	swapHeaders(h, []Login{gh})
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(h.Get("Authorization"), "Basic "))
	if string(raw) != "x-access-token:gho_REAL" || h.Get("X-Other") != "token gho_REAL" {
		t.Errorf("swapped: %s, %s", raw, h.Get("X-Other"))
	}
	h = http.Header{"Authorization": {"token gho_PH"}}
	swapHeaders(h, nil)
	if h.Get("Authorization") != "token gho_PH" {
		t.Error("swapped for a login it wasn't given")
	}
}

func TestLoginsByGrantAndHost(t *testing.T) {
	p := NewProxy()
	p.table = Table{
		Logins: []Login{{Name: "claude", Adapter: "claude", Hosts: []string{"anthropic.com"}}, {Name: "gh", Adapter: "gh", Hosts: []string{"github.com"}}},
		Grants: map[string][]string{"a": {"gh"}, "b": {"*"}},
	}
	names := func(box string) string {
		ls, _ := p.logins(box)
		var n []string
		for _, l := range ls {
			n = append(n, l.Adapter)
		}
		return strings.Join(n, ",")
	}
	if names("a") != "gh" || names("b") != "claude,gh" || names("c") != "" {
		t.Errorf("a=%s b=%s c=%s", names("a"), names("b"), names("c"))
	}
	l := p.table.Logins[0]
	if !l.ForHost("api.anthropic.com") || l.ForHost("evilanthropic.com") || l.ForHost("anthropic.com.evil.io") {
		t.Error("ForHost")
	}
}

// A refresh's answer must carry no real token, and say how the login changed.
func TestPlaceholdered(t *testing.T) {
	l := Login{Name: "claude", Adapter: "claude", Secrets: []Secret{
		{Key: "accesstoken", Real: "old-access", Placeholders: []string{"ph-access"}},
		{Key: "refreshtoken", Real: "old-refresh", Placeholders: []string{"ph-refresh"}},
	}}
	m := map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "id_token": "new-id", "expires_in": 28800.0}
	changes := Placeholdered(m, l)
	if m["access_token"] != "ph-access" || m["refresh_token"] != "ph-refresh" || m["id_token"] == "new-id" || m["expires_in"] != 28800.0 {
		t.Errorf("answer: %v", m)
	}
	if len(changes) != 3 {
		t.Errorf("changes: %+v", changes)
	}
}

func TestBlocked(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "169.254.169.254", "::1", "fd00::1", "0.0.0.0"} {
		if !Blocked(net.ParseIP(ip)) {
			t.Errorf("%s reachable", ip)
		}
	}
	if Blocked(net.ParseIP("1.1.1.1")) {
		t.Error("1.1.1.1 blocked")
	}
}

// MCP config mixes keys with settings; only keys may become placeholders, or the
// server breaks.
func TestLooksSecret(t *testing.T) {
	for v, want := range map[string]bool{
		"lin_api_" + strings.Repeat("x", 32): true,
		"Bearer " + strings.Repeat("y", 40):  true,
		"ghp_" + strings.Repeat("z", 36):     true,
		"true":                               false,
		"debug":                              false,
		"/home/dev/.config/server.json":      false,
		"https://mcp.linear.app/sse":         false,
		"some value with spaces in it":       false,
	} {
		if LooksSecret(v) != want {
			t.Errorf("LooksSecret(%q) = %v", v, !want)
		}
	}
}
