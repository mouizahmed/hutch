package keys

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func verifyIdentity(t *testing.T, token string, seed []byte, box, project, host string, now time.Time) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	pub, err := IdentityPublic(seed)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.RawURLEncoding.DecodeString(pub.Keys[0].X)
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if !ed25519.Verify(ed25519.PublicKey(key), []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("invalid signature")
	}
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
		Project string `json:"project"`
		Host    string `json:"aud"`
		Expiry  int64  `json:"exp"`
		Issued  int64  `json:"iat"`
		NotYet  int64  `json:"nbf"`
		ID      string `json:"jti"`
	}
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != pub.Issuer || claims.Subject != box || claims.Project != project || claims.Host != host ||
		claims.Issued != now.Unix() || claims.NotYet != now.Unix() || claims.Expiry != now.Add(5*time.Minute).Unix() || claims.ID == "" {
		t.Fatalf("claims: %+v", claims)
	}
}

func TestBoxIdentity(t *testing.T) {
	seed := []byte(strings.Repeat("s", 32))
	now := time.Unix(1000, 0)
	p := NewProxy()
	p.table = Table{IdentitySeed: seed, IdentityHosts: []string{"service.example"}, Projects: map[string]string{"box-a": "project-a", "box-b": "project-b"}}
	for _, box := range []string{"box-a", "box-b"} {
		h := http.Header{IdentityHeader: {"pretend-to-be-another-box"}, "Authorization": {"Bearer service-key"}}
		if err := p.addIdentity(h, box, "service.example", now); err != nil {
			t.Fatal(err)
		}
		verifyIdentity(t, h.Get(IdentityHeader), seed, box, p.table.Projects[box], "service.example", now)
		if h.Get("Authorization") != "Bearer service-key" {
			t.Fatal("changed the service's authentication")
		}
	}
	for _, host := range []string{"other.example", "sub.service.example", "service.example.evil"} {
		h := http.Header{IdentityHeader: {"forged"}}
		if err := p.addIdentity(h, "box-a", host, now); err != nil || h.Get(IdentityHeader) != "" || p.identifies("box-a", host) {
			t.Fatalf("identity leaked to %s", host)
		}
	}
	h := http.Header{}
	if err := p.addIdentity(h, "unknown", "service.example", now); err != nil || h.Get(IdentityHeader) != "" {
		t.Fatal("unknown box got an identity")
	}
	first, _ := identityToken(seed, "box-a", "project-a", "service.example", now)
	second, _ := identityToken(seed, "box-a", "project-a", "service.example", now)
	if first == second {
		t.Fatal("reused token ID")
	}
}

// Requests on one TLS connection must see rotated credentials and revoked
// grants. The identity comes from the proxy's box, even if the caller spoofs it.
func TestProxyLiveGrantsAndIdentity(t *testing.T) {
	seed := []byte(strings.Repeat("s", 32))
	received := make(chan http.Header, 3)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	p := NewProxy()
	cert, key, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	table := Table{CACert: cert, CAKey: key, IdentitySeed: seed, IdentityHosts: []string{"service.example"}, Projects: map[string]string{"box": "project"},
		Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{{Name: "agent", Adapter: "agent", Hosts: []string{"service.example"}, Secrets: []Secret{{Real: "real-one", Placeholders: []string{"placeholder"}}}}}}
	if err := p.Set(table); err != nil {
		t.Fatal(err)
	}
	// Only the test's upstream dial is substituted; the real TLS proxy handles
	// requests from the simulated box, using a certificate signed by its CA.
	upTLS := upstream.Client().Transport.(*http.Transport).TLSClientConfig
	p.transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&tls.Dialer{Config: upTLS}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		p.intercept(serverConn, "box", "service.example", "service.example:443", nil)
		close(done)
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	dials := 0
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials++
		return clientConn, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for i, want := range []string{"Bearer real-one", "Bearer real-two", "Bearer placeholder"} {
		if i == 1 {
			table.Logins = []Login{{Name: "agent", Adapter: "agent", Hosts: []string{"service.example"}, Secrets: []Secret{{Real: "real-two", Placeholders: []string{"placeholder"}}}}}
		} else if i == 2 {
			table.Grants = map[string][]string{}
		}
		if err := p.Set(table); err != nil {
			t.Fatal(err)
		}
		r, _ := http.NewRequest(http.MethodGet, "https://service.example/path", nil)
		r.Header.Set("Authorization", "Bearer placeholder")
		r.Header.Set(IdentityHeader, "forged")
		before := time.Now().Unix()
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		h := <-received
		if h.Get("Authorization") != want {
			t.Fatalf("request %d: %q, want %q", i, h.Get("Authorization"), want)
		}
		// Requests may cross a wall-clock second; verify using the token's iat.
		parts := strings.Split(h.Get(IdentityHeader), ".")
		if len(parts) != 3 {
			t.Fatal("no proxy identity")
		}
		data, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Iat int64 `json:"iat"`
		}
		json.Unmarshal(data, &claims)
		if claims.Iat < before || claims.Iat > time.Now().Unix() {
			t.Fatal("identity is not fresh")
		}
		verifyIdentity(t, h.Get(IdentityHeader), seed, "box", "project", "service.example", time.Unix(claims.Iat, 0))
	}
	if dials != 1 {
		t.Fatalf("test needs one TLS connection, got %d", dials)
	}
	transport.CloseIdleConnections()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not release its connection")
	}
}
