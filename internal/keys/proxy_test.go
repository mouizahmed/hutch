package keys

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// proxied runs the proxy for box "box" towards upstream, posing as host, and
// returns a client that talks to it as the box would.
func proxied(t *testing.T, table Table, upstream *httptest.Server, host string) (*Proxy, *http.Client) {
	t.Helper()
	p := NewProxy()
	cert, key, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	table.CACert, table.CAKey = cert, key
	if err := p.Set(table); err != nil {
		t.Fatal(err)
	}
	upTLS := upstream.Client().Transport.(*http.Transport).TLSClientConfig
	p.transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&tls.Dialer{Config: upTLS}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(context.Context, string, string) (net.Conn, error) {
		s, c := net.Pipe()
		go p.intercept(s, "box", host, host+":443", nil)
		return c, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return p, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

const realRefresh = "rt-REAL-refresh-token-value-0123456789"

func refreshLogin(tokenURL string) Login {
	return Login{Name: "agent", Adapter: "agent", Hosts: []string{"auth.example"}, TokenURL: tokenURL, Secrets: []Secret{
		{Key: "accesstoken", Real: "at-REAL-access-token-value-0123456789", Placeholders: []string{"at-hutch-access"}},
		{Key: "refreshtoken", Real: realRefresh, Placeholders: []string{"rt-hutch-refresh"}},
	}}
}

// A refresh-shaped body anywhere but the token URL, or headers a host echoes,
// must not get a real value back to the box.
func TestNoRealValueEchoed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<p>" + r.Header.Get("Authorization") + " " + string(body) + "</p>"))
	}))
	defer upstream.Close()
	_, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{refreshLogin("https://auth.example/oauth/token")}}, upstream, "auth.example")
	for _, path := range []string{"/markdown", "/oauth/token"} {
		body := `{"grant_type":"refresh_token","refresh_token":"at-hutch-access","text":"rt-hutch-refresh at-hutch-access"}`
		r, _ := http.NewRequest(http.MethodPost, "https://auth.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer at-hutch-access")
		r.Header.Set("Origin", "rt-hutch-refresh")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		all := string(got) + resp.Header.Get("Access-Control-Allow-Origin")
		if strings.Contains(all, "REAL") {
			t.Errorf("%s: a real value reached the box: %s", path, all)
		}
		if !strings.Contains(string(got), "Bearer at-hutch-access") {
			t.Errorf("%s: the echoed auth header wasn't scrubbed back to its placeholder: %s", path, got)
		}
	}
}

// A refresh through the token URL gets the box placeholders, keeps working when
// storing it fails at first, and goes upstream once for boxes asking together.
func TestRefresh(t *testing.T) {
	var mu sync.Mutex
	upstreamRefreshes, nextRefresh := 0, realRefresh
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/oauth/token" || req["refresh_token"] != nextRefresh {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant","sent":"` + req["refresh_token"] + `"}`))
			return
		}
		upstreamRefreshes++
		nextRefresh = "rt-REAL-rotated-refresh-token-012345678"
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-REAL-new-access-token-value-01234", "refresh_token": nextRefresh, "expires_in": 3600})
	}))
	defer upstream.Close()
	p, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}}, Reuse: time.Minute,
		Logins: []Login{refreshLogin("https://auth.example/oauth/token")}}, upstream, "auth.example")
	var stored [][]Change
	fails := 1
	p.Refreshed = func(ctx context.Context, login string, changes []Change) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return io.ErrUnexpectedEOF
		}
		stored = append(stored, changes)
		return nil
	}
	refresh := func() map[string]any {
		r, _ := http.NewRequest(http.MethodPost, "https://auth.example/oauth/token", strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"rt-hutch-refresh"}`))
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(data, &m) != nil || bytes.Contains(data, []byte("REAL")) {
			t.Fatalf("refresh answered %d: %s", resp.StatusCode, data)
		}
		return m
	}
	first, second := refresh(), refresh()
	if first["refresh_token"] != "rt-hutch-refresh" || first["access_token"] != second["access_token"] {
		t.Errorf("answers: %v, %v", first, second)
	}
	mu.Lock()
	if upstreamRefreshes != 1 {
		t.Errorf("upstream refreshed %d times", upstreamRefreshes)
	}
	mu.Unlock()
	// Storing failed once: the proxy kept the rotated token, and keeps it over
	// an older table until the server's has it.
	if err := p.Set(Table{CACert: p.table.CACert, CAKey: p.table.CAKey, Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{refreshLogin("https://auth.example/oauth/token")}}); err != nil {
		t.Fatal(err)
	}
	if sec, _ := p.allLogins()[0].refreshSecret(); sec.Real != "rt-REAL-rotated-refresh-token-012345678" {
		t.Errorf("an older table dropped the rotated refresh token: %s", sec.Real)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(stored)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh was never stored")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Credentials only go to the host the box connected to.
func TestHostMustMatchConnect(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer upstream.Close()
	_, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{refreshLogin("")}}, upstream, "auth.example")
	r, _ := http.NewRequest(http.MethodGet, "https://auth.example/", nil)
	r.Host = "other.example"
	r.Header.Set("Authorization", "Bearer at-hutch-access")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status %d for a mismatched Host", resp.StatusCode)
	}
}

// A real value split across reads is still replaced, and bytes that can't be
// part of one aren't held back.
func TestScrubReader(t *testing.T) {
	s := scrubberOf([]Login{refreshLogin("")})
	in := "data: x " + realRefresh + " y\n\ndata: z\n\n"
	got, err := io.ReadAll(s.reader(iotest.OneByteReader(strings.NewReader(in))))
	if err != nil || strings.Contains(string(got), "REAL") || !strings.Contains(string(got), "rt-hutch-refresh") {
		t.Errorf("scrubbed: %q, %v", got, err)
	}
	pr, pw := io.Pipe()
	r := s.reader(pr)
	go pw.Write([]byte("data: done\n\n"))
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "data: done\n\n" {
		t.Errorf("held back %q", buf[:n])
	}
	pw.Close()
}
