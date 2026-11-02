package keys

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// viaLink has a lead's proxy sign and refresh for a worker's, through JSON as
// over the link.
type viaLink struct{ lead *Proxy }

func relay[T any](v T, err error) (T, error) {
	var out T
	if err != nil {
		return out, err
	}
	data, _ := json.Marshal(v)
	return out, json.Unmarshal(data, &out)
}

func (l viaLink) Cert(_ context.Context, host string, pub []byte) ([]byte, error) {
	return relay(l.lead.SignCert(host, pub))
}

func (l viaLink) Identity(_ context.Context, box, host string) (Token, error) {
	return relay(l.lead.SignIdentity(box, host))
}

func (l viaLink) Refresh(ctx context.Context, c RefreshCall) (RefreshAnswer, error) {
	c, _ = relay(c, nil)
	return relay(l.lead.Refresh(ctx, c))
}

// Workers without the CA's key serve their boxes with the lead's certificates,
// and a refresh through one is used by the other: the lead refreshes with the
// token the first rotated, never a stale one.
func TestRefreshThroughLead(t *testing.T) {
	var mu sync.Mutex
	n, refresh, access := 0, realRefresh, "at-REAL-access-token-value-0123456789"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api" {
			if r.Header.Get("Authorization") != "Bearer "+access {
				w.WriteHeader(http.StatusUnauthorized)
			}
			return
		}
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["refresh_token"] != refresh {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n++
		refresh, access = fmt.Sprintf("rt-REAL-rotated-%d-0123456789012345", n), fmt.Sprintf("at-REAL-access-%d-0123456789012345", n)
		json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh})
	}))
	defer upstream.Close()
	upTLS := upstream.Client().Transport.(*http.Transport).TLSClientConfig
	toUpstream := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&tls.Dialer{Config: upTLS}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}

	cert, key, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	logins := []Login{refreshLogin("https://auth.example/oauth/token")}
	grants := map[string][]string{"box-a": {"agent"}, "box-b": {"agent"}}
	lead, wa, wb := NewProxy(), NewProxy(), NewProxy()
	lead.transport.DialTLSContext = toUpstream
	set := func() {
		mu.Lock()
		l := logins
		mu.Unlock()
		lead.Set(Table{CACert: cert, CAKey: key, Grants: grants, Logins: l})
		wa.Set(Table{CACert: cert, Grants: map[string][]string{"box-a": {"agent"}}, Logins: l})
		wb.Set(Table{CACert: cert, Grants: map[string][]string{"box-b": {"agent"}}, Logins: l})
	}
	// The server stores a refresh and pushes the new table to every machine.
	lead.Refreshed = func(_ context.Context, _ string, changes []Change) error {
		mu.Lock()
		logins = []Login{logins[0]}
		logins[0].Secrets = ApplyChanges(logins[0].Secrets, changes)
		mu.Unlock()
		set()
		return nil
	}
	set()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	client := func(p *Proxy, box string) *http.Client {
		p.Lead = viaLink{lead}
		p.transport.DialTLSContext = toUpstream
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, DialContext: func(context.Context, string, string) (net.Conn, error) {
			s, c := net.Pipe()
			go p.intercept(s, box, "auth.example", "auth.example:443", nil)
			return c, nil
		}}
		t.Cleanup(transport.CloseIdleConnections)
		return &http.Client{Transport: transport, Timeout: 5 * time.Second}
	}
	a, b := client(wa, "box-a"), client(wb, "box-b")
	do := func(c *http.Client, r *http.Request) []byte {
		t.Helper()
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || strings.Contains(string(data), "REAL") {
			t.Fatalf("%s answered %d: %s", r.URL.Path, resp.StatusCode, data)
		}
		return data
	}
	refreshOn := func(c *http.Client) {
		t.Helper()
		r, _ := http.NewRequest(http.MethodPost, "https://auth.example/oauth/token", strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"rt-hutch-refresh"}`))
		r.Header.Set("Content-Type", "application/json")
		do(c, r)
	}
	api := func(c *http.Client) {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, "https://auth.example/api", nil)
		r.Header.Set("Authorization", "Bearer at-hutch-access")
		do(c, r)
	}

	refreshOn(a)
	api(a)
	deadline := time.Now().Add(5 * time.Second)
	for sec, _ := wb.allLogins()[0].refreshSecret(); sec.Real == realRefresh; sec, _ = wb.allLogins()[0].refreshSecret() {
		if time.Now().After(deadline) {
			t.Fatal("the other worker never got the refreshed login")
		}
		time.Sleep(20 * time.Millisecond)
	}
	api(b)
	refreshOn(b)
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Fatalf("upstream refreshed %d times, want 2", n)
	}
}
