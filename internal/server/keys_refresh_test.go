package server

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
)

func TestRefreshStoreAtomicAndReplayable(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "refresh.db"), durable.NewKey())
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s := &Server{store: st}
	files, _ := json.Marshal([]guestproto.File{{Path: "~/.codex/auth.json", Data: []byte(`{"tokens":{"access_token":"access-a","refresh_token":"refresh-a"}}`)}})
	if _, _, err := st.putLayerPart(credentialsPrefix+"codex", 0, files); err != nil {
		t.Fatal(err)
	}
	if err := s.putLoginSecrets("codex", []keys.Secret{
		{Key: "accesstoken", Real: "access-a", Placeholders: []string{"access-ph"}},
		{Key: "refreshtoken", Real: "refresh-a", Placeholders: []string{"refresh-ph"}},
	}); err != nil {
		t.Fatal(err)
	}
	p := api.KeysRefreshedParams{Adapter: "codex", Changes: []keys.Change{
		{Key: "accesstoken", Old: "access-a", New: "access-b", Placeholder: "access-ph"},
		{Key: "refreshtoken", Old: "refresh-a", New: "refresh-b", Placeholder: "refresh-ph"},
	}}
	if _, err := st.db.Exec(`CREATE TRIGGER fail_refresh BEFORE UPDATE ON user_layer WHEN NEW.part = 'keyproxy/codex' BEGIN SELECT RAISE(ABORT, 'synthetic interrupted write'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.storeRefresh(p); err == nil {
		t.Fatal("injected write failure was ignored")
	}
	unchanged, err := st.layerPart(credentialsPrefix + "codex")
	if err != nil || unchanged.version != 1 || !bytes.Equal(unchanged.content, files) {
		t.Fatalf("failed refresh partially changed stored credentials: version=%d err=%v", unchanged.version, err)
	}
	if _, err := st.db.Exec(`DROP TRIGGER fail_refresh`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.storeRefresh(p); err != nil {
			t.Fatal(err)
		}
	}
	secrets, err := s.loginSecrets("codex")
	if err != nil || len(secrets) != 2 {
		t.Fatalf("replayed store duplicated secrets: %v err=%v", secrets, err)
	}
	stored, err := st.layerPart(credentialsPrefix + "codex")
	if err != nil || bytes.Contains(stored.content, []byte("access-a")) || bytes.Contains(stored.content, []byte("refresh-a")) {
		t.Fatal("successful refresh left the old login in the credential files")
	}
}
