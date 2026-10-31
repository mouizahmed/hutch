package keys

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request can finish after another box rotated the login. Its old header
// values must still be removed from the response.
func TestResponseDuringRotation(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer upstream.Close()
	l := refreshLogin("")
	table := Table{Grants: map[string][]string{"box": {"agent"}}, Logins: []Login{l}}
	p, client := proxied(t, table, upstream, "auth.example")
	done := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "https://auth.example/", nil)
		req.Header.Set("Authorization", "Bearer at-hutch-access")
		resp, err := client.Do(req)
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		done <- string(data)
	}()
	<-arrived
	table.CACert, table.CAKey = p.table.CACert, p.table.CAKey
	table.Logins = []Login{refreshLogin("")}
	table.Logins[0].Secrets[0].Real = "at-REAL-rotated-access-value-0123456789"
	if err := p.Set(table); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-done; got != "Bearer at-hutch-access" {
		t.Errorf("response after rotation: %q", got)
	}
}

// Token fields are not the only places a provider can repeat values. Scrub
// successful refresh responses after learning the newly issued tokens too.
func TestRefreshResponseExtraFields(t *testing.T) {
	const nextAccess = "at-REAL-next-access-value-0123456789"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  nextAccess,
			"refresh_token": "rt-REAL-next-refresh-value-0123456789",
			"details":       map[string]string{"previous": realRefresh, "issued": nextAccess},
		})
	}))
	defer upstream.Close()
	p, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{refreshLogin("https://auth.example/oauth/token")}}, upstream, "auth.example")
	p.Refreshed = func(context.Context, string, []Change) error { return nil }
	r, _ := http.NewRequest(http.MethodPost, "https://auth.example/oauth/token", strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"rt-hutch-refresh"}`))
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(data), "REAL") {
		t.Errorf("refresh exposed a real value: %d %s", resp.StatusCode, data)
	}
}

// A lost acknowledgement can resend a successful store. Retrying it, including
// after a later refresh, must never recreate an older secret or shadow its alias.
func TestApplyChangesReplay(t *testing.T) {
	first := Change{Key: "accesstoken", Old: "access-a", New: "access-b", Placeholder: "access-ph"}
	second := Change{Key: "accesstoken", Old: "access-b", New: "access-c", Placeholder: "access-ph"}
	secrets := []Secret{{Key: first.Key, Real: first.Old, Placeholders: []string{first.Placeholder}}}
	for _, c := range []Change{first, first, second, first, second} {
		secrets = ApplyChanges(secrets, []Change{c})
	}
	if len(secrets) != 1 || secrets[0].Real != second.New {
		t.Fatalf("refresh retries duplicated or regressed the secret: %+v", secrets)
	}
}

// Tables confirming multiple refreshes must clear the pending overlay. Applying
// that overlay to a confirmed table must not add old values back.
func TestPendingRepeatedRefreshes(t *testing.T) {
	p := NewProxy()
	cert, key, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	table := Table{CACert: cert, CAKey: key, Logins: []Login{refreshLogin("")}}
	if err := p.Set(table); err != nil {
		t.Fatal(err)
	}
	first := Change{Key: "refreshtoken", Old: realRefresh, New: "refresh-b", Placeholder: "rt-hutch-refresh"}
	second := Change{Key: "refreshtoken", Old: first.New, New: "refresh-c", Placeholder: first.Placeholder}
	p.pending["agent"] = []Change{first, second}
	table.Logins = []Login{refreshLogin("")}
	table.Logins[0].Secrets = ApplyChanges(table.Logins[0].Secrets, []Change{first, second})
	if err := p.Set(table); err != nil {
		t.Fatal(err)
	}
	if len(p.pending) != 0 || len(p.allLogins()[0].Secrets) != 2 {
		t.Errorf("confirmed refreshes left a stale overlay or duplicates: pending=%v secrets=%v", p.pending, p.allLogins()[0].Secrets)
	}
}

// A retry after losing the store's acknowledgement must finish before the next
// refresh is saved, even though both boxes already have their answers.
func TestRefreshStoreOrder(t *testing.T) {
	p := NewProxy()
	first, second := []Change{{New: "first"}}, []Change{{New: "second"}}
	st := &refreshState{}
	attempts := make(chan string, 4)
	calls := 0
	p.Refreshed = func(_ context.Context, _ string, changes []Change) error {
		attempts <- changes[0].New
		calls++
		if calls == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	p.queueStore(st, "agent", first)
	if got := <-attempts; got != "first" {
		t.Fatal(got)
	}
	p.queueStore(st, "agent", second)
	for _, want := range []string{"first", "second"} {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("store order: got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("refresh queue stopped")
		}
	}
}

func TestRefreshSurvivesClientCancellation(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"next-access","refresh_token":"next-refresh"}`)
	}))
	defer upstream.Close()
	p, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}},
		Logins: []Login{refreshLogin("https://auth.example/oauth/token")}}, upstream, "auth.example")
	stored := make(chan []Change, 1)
	p.Refreshed = func(_ context.Context, _ string, changes []Change) error {
		stored <- changes
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://auth.example/oauth/token", strings.NewReader(`{"grant_type":"refresh_token","refresh_token":"rt-hutch-refresh"}`))
	r.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(r)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-arrived
	cancel()
	if err := <-done; err == nil {
		t.Fatal("client cancellation had no effect")
	}
	close(release)
	select {
	case changes := <-stored:
		found := false
		for _, c := range changes {
			found = found || c.Key == "refreshtoken" && c.New == "next-refresh"
		}
		if !found {
			t.Fatal("cancelled client lost the provider's rotated token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client cancellation interrupted refresh persistence")
	}
}

func TestResponseEncodingsAndTrailers(t *testing.T) {
	for _, kind := range []string{"basic", "gzip-range", "trailer"} {
		t.Run(kind, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "basic":
					io.WriteString(w, r.Header.Get("Authorization"))
				case "gzip-range":
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzip.NewWriter(w)
					io.WriteString(gz, realRefresh)
					gz.Close()
				case "trailer":
					w.Header().Set("Trailer", "X-Token")
					io.WriteString(w, "done")
					w.Header().Set("X-Token", realRefresh)
				}
			}))
			defer upstream.Close()
			_, client := proxied(t, Table{Grants: map[string][]string{"box": {"agent"}},
				Logins: []Login{refreshLogin("")}}, upstream, "auth.example")
			r, _ := http.NewRequest(http.MethodGet, "https://auth.example/", nil)
			r.SetBasicAuth("agent", "rt-hutch-refresh")
			if kind == "gzip-range" {
				r.Header.Set("Range", "bytes=0-")
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body io.Reader = resp.Body
			if resp.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(body)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				body = gz
			}
			data, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "basic" {
				data, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(string(data), "Basic "))
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(string(data), "REAL") || strings.Contains(resp.Trailer.Get("X-Token"), "REAL") {
				t.Fatalf("response disclosed a real token: %q trailer=%v", data, resp.Trailer)
			}
		})
	}
}
