package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// A worker gets only the logins of the boxes it hosts, never the CA's key or the
// identity seed, and the lead signs only for what it got.
func TestWorkerKeysScoped(t *testing.T) {
	ctx := t.Context()
	s, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.store.config("user")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.putConfig("user", user.Version, "keys.identity_hosts = [\"id.example\"]\n"); err != nil {
		t.Fatal(err)
	}
	set := agents.Builtin()
	for name, file := range map[string]string{
		"claude": `{"claudeAiOauth":{"accessToken":"real-claude"}}`,
		"codex":  `{"tokens":{"access_token":"real-codex"}}`,
	} {
		data, _ := json.Marshal([]guestproto.File{{Path: set[name].Credentials[0].File, Data: []byte(file)}})
		if _, _, err := s.store.putLayerPart(credentialsPrefix+name, 0, data); err != nil {
			t.Fatal(err)
		}
		if err := s.syncSecrets(set[name], []guestproto.File{{Path: set[name].Credentials[0].File, Data: []byte(file)}}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	got := map[string]keys.Table{}
	for _, id := range []string{"w-a", "w-b"} {
		if err := s.store.ensureWorker(id, id); err != nil {
			t.Fatal(err)
		}
		left, right := net.Pipe()
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = io.Discard
		client, err := yamux.Client(left, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		worker, err := yamux.Server(right, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer worker.Close()
		go rpc.Serve(ctx, worker, rpc.Mux{api.WorkerKeys: func(_ context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var tb keys.Table
			err := json.Unmarshal(params, &tb)
			mu.Lock()
			got[id] = tb
			mu.Unlock()
			return nil, err
		}}, nil)
		s.mu.Lock()
		s.workers[id] = &workerConn{id: id, sess: client, projects: map[string]bool{}}
		s.mu.Unlock()
	}
	for _, b := range []struct{ id, worker, project, login string }{{"a1", "w-a", "p-a", "claude"}, {"b1", "w-b", "p-b", "codex"}} {
		if err := s.store.putBox(api.Box{ID: b.id, Worker: b.worker, Project: b.project, State: "running", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.putMeta(b.id, boxMeta{project: b.project}); err != nil {
			t.Fatal(err)
		}
		if err := s.store.setBoxCredentials(b.id, []string{b.login}); err != nil {
			t.Fatal(err)
		}
	}
	s.pushKeys(ctx, nil)

	mu.Lock()
	a := got["w-a"]
	mu.Unlock()
	if len(a.CAKey) != 0 || len(a.IdentitySeed) != 0 || len(a.CACert) == 0 {
		t.Fatal("a worker got the CA's key or the identity seed")
	}
	if len(a.Logins) != 1 || a.Logins[0].Name != "claude" || len(a.Grants) != 1 || a.Projects["a1"] != "p-a" || len(a.Projects) != 1 {
		t.Fatalf("w-a got logins %v, grants %v, boxes %v; want claude for a1 only", a.Logins, a.Grants, a.Projects)
	}
	// Hosting p-b's project brings its boxes' logins.
	s.hostProject(ctx, s.workers["w-a"], "p-b")
	mu.Lock()
	a = got["w-a"]
	mu.Unlock()
	if len(a.Logins) != 2 || a.Projects["b1"] != "p-b" {
		t.Fatalf("after hosting p-b, w-a got logins %v, boxes %v", a.Logins, a.Projects)
	}

	call := func(worker, method string, params, out any) error {
		p, _ := json.Marshal(params)
		res, err := s.workerMux(worker)[method](ctx, p, nil)
		if err == nil && out != nil {
			b, _ := json.Marshal(res)
			err = json.Unmarshal(b, out)
		}
		return err
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pub, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	codexHost := set["codex"].Hosts[0]
	if err := call("w-b", api.WorkerKeysCert, api.KeysCertParams{Host: "evil.example", Public: pub}, nil); err == nil {
		t.Fatal("the lead signed a host no login of the worker's is for")
	}
	var der []byte
	if err := call("w-b", api.WorkerKeysCert, api.KeysCertParams{Host: codexHost, Public: pub}, &der); err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	block, _ := pem.Decode(a.CACert)
	ca, _ := x509.ParseCertificate(block.Bytes)
	if err != nil || leaf.CheckSignatureFrom(ca) != nil || leaf.VerifyHostname(codexHost) != nil {
		t.Fatalf("the lead's certificate for %s: %v", codexHost, err)
	}

	if err := call("w-b", api.WorkerKeysIdentity, api.KeysIdentityParams{Box: "a1", Host: "id.example"}, nil); err == nil {
		t.Fatal("the lead signed an identity for a box on another machine")
	}
	var tok keys.Token
	if err := call("w-b", api.WorkerKeysIdentity, api.KeysIdentityParams{Box: "b1", Host: "id.example"}, &tok); err != nil || strings.Count(tok.Token, ".") != 2 {
		t.Fatalf("b1's identity: %q %v", tok.Token, err)
	}
	if err := call("w-b", api.WorkerKeysRefresh, keys.RefreshCall{Box: "a1", Login: "claude", HostPort: "x:443"}, nil); err == nil || !strings.Contains(err.Error(), "isn't on this machine") {
		t.Fatalf("a refresh for a box on another machine: %v", err)
	}
}
