package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// A proxy controls new requests; it cannot take raw secrets out of a source
// snapshot. This also covers missing provenance and wildcard grants.
func TestInheritedLogins(t *testing.T) {
	for _, tt := range []struct {
		name       string
		had, creds []string
		allowed    bool
	}{
		{"same", []string{"claude", "gh"}, []string{"gh", "claude"}, true},
		{"broader", []string{"gh"}, []string{"gh", "claude"}, true},
		{"narrower", []string{"gh", "claude"}, []string{"gh"}, false},
		{"wildcard narrowed", []string{"*"}, []string{"gh"}, false},
		{"all", []string{"*"}, []string{"*"}, true},
		{"none", []string{}, []string{}, true},
		{"unknown", nil, []string{}, false},
		{"unknown broad", nil, []string{"*"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := inheritedLogins(tt.had, tt.creds)
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v err=%v", tt.allowed, err)
			}
		})
	}
}

func TestManagedCapturePreservesStoredLogin(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "capture.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	if err := st.ensureWorker("fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := st.putBox(api.Box{ID: "capture", Worker: "fixture", State: "running"}); err != nil {
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go rpc.Serve(ctx, worker, rpc.Mux{api.BoxExec: func(_ context.Context, _ json.RawMessage, out *rpc.Out) (any, error) {
		return rpc.ExitCode(0), out.Stdout([]byte(`{"tokens":{"access_token":"known-placeholder"}}`))
	}}, nil)
	s := &Server{store: st, workers: map[string]*workerConn{"fixture": {id: "fixture", sess: client}}}
	a := agents.Builtin()["codex"]
	data, _ := json.Marshal([]guestproto.File{{Path: a.Credentials[0].File, Data: []byte(`{"tokens":{"access_token":"real-provider-token"}}`)}})
	if _, _, err := st.putLayerPart(credentialsPrefix+a.Name, 0, data); err != nil {
		t.Fatal(err)
	}
	if err := s.putLoginSecrets(a.Name, []keys.Secret{{Key: "accesstoken", Real: "real-provider-token", Placeholders: []string{"known-placeholder"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := st.layerPart(credentialsPrefix + a.Name)
	if err != nil {
		t.Fatal(err)
	}
	beforeProxy, err := st.layerPart(keysPrefix + a.Name)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(api.CaptureParams{Box: "capture", Adapter: a.Name})
	if _, err := s.userCapture(ctx, params, nil); err == nil || !strings.Contains(err.Error(), "managed login") {
		t.Fatalf("managed placeholder capture was accepted: %v", err)
	}
	after, err := st.layerPart(credentialsPrefix + a.Name)
	if err != nil {
		t.Fatal(err)
	}
	afterProxy, err := st.layerPart(keysPrefix + a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if before.version != after.version || string(before.content) != string(after.content) ||
		beforeProxy.version != afterProxy.version || string(beforeProxy.content) != string(afterProxy.content) {
		t.Fatal("refused capture modified a working stored login or proxy tokens")
	}
}

func TestCaptureRefusesManagedPlaceholdersAndMetadata(t *testing.T) {
	const real = "synthetic-provider-token-never-print"
	const placeholder = "synthetic-placeholder-never-print"
	const oldPlaceholder = "synthetic-old-placeholder-never-print"
	known := []keys.Secret{{Key: "access_token", Real: real, Placeholders: []string{placeholder, oldPlaceholder}}}
	a := agents.Builtin()["codex"]
	for _, tt := range []struct {
		name, token, want string
	}{
		{"fresh real login", real, ""},
		{"current placeholder", placeholder, "managed login"},
		{"older placeholder", oldPlaceholder, "managed login"},
		{"metadata without a token", "", "no token fields"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"account": "fixture", "tokens": map[string]string{"access_token": tt.token}})
			err := checkCapturedLogin(a, []guestproto.File{{Path: "~/.codex/auth.json", Data: data}}, known)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
			for _, value := range []string{real, placeholder, oldPlaceholder} {
				if strings.Contains(err.Error(), value) {
					t.Fatal("capture error disclosed a credential value")
				}
			}
		})
	}
}
