package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The keys proxy's state lives in the user layer, sealed: the machine's CA, and
// per login its secrets with their placeholders ([keys]).
const keysPrefix = "keyproxy/"

type caPart struct {
	Cert []byte `json:"cert"`
	Key  []byte `json:"key"`
}

// keyCA returns the machine's CA, made on first use.
func (s *Server) keyCA() (cert, key []byte, err error) {
	p, err := s.store.layerPart(keysPrefix + "ca")
	if err != nil {
		return nil, nil, err
	}
	var c caPart
	if len(p.content) > 0 {
		if err := json.Unmarshal(p.content, &c); err != nil {
			return nil, nil, err
		}
		return c.Cert, c.Key, nil
	}
	if c.Cert, c.Key, err = keys.NewCA(); err != nil {
		return nil, nil, err
	}
	data, _ := json.Marshal(c)
	if _, conflict, err := s.store.putLayerPart(keysPrefix+"ca", 0, data); err != nil {
		return nil, nil, err
	} else if conflict {
		return s.keyCA()
	}
	return c.Cert, c.Key, nil
}

func (s *Server) loginSecrets(adapter string) ([]keys.Secret, error) {
	p, err := s.store.layerPart(keysPrefix + adapter)
	if err != nil || len(p.content) == 0 {
		return nil, err
	}
	var out []keys.Secret
	return out, json.Unmarshal(p.content, &out)
}

func (s *Server) putLoginSecrets(adapter string, secrets []keys.Secret) error {
	data, _ := json.Marshal(secrets)
	_, _, err := s.store.putLayerPart(keysPrefix+adapter, -1, data)
	return err
}

// syncSecrets finds a login's secrets in its captured files. A secret that
// changed (a new login) keeps its placeholders, so the copies boxes have stay
// valid; a JWT gets a new first one, carrying its new claims.
func (s *Server) syncSecrets(a *agents.Adapter, files []guestproto.File) error {
	old, err := s.loginSecrets(a.Name)
	if err != nil {
		return err
	}
	found := map[string]string{} // real → key
	for _, src := range a.Credentials {
		for _, f := range files {
			if f.Path == src.File && len(src.Secrets) > 0 {
				maps.Copy(found, keys.Find(f.Path, f.Data, src.Secrets))
			}
		}
	}
	reals := slices.SortedFunc(maps.Keys(found), func(x, y string) int { return cmp.Or(cmp.Compare(found[x], found[y]), cmp.Compare(x, y)) })
	taken := map[int]bool{}
	var out []keys.Secret
	for _, real := range reals {
		sec := keys.Secret{Key: found[real], Real: real}
		i := slices.IndexFunc(old, func(o keys.Secret) bool { return o.Real == real })
		if i < 0 {
			i = slices.IndexFunc(old, func(o keys.Secret) bool { return o.Key == sec.Key && o.Real != real })
		}
		changed := false
		if i >= 0 && !taken[i] {
			taken[i] = true
			sec.Placeholders, changed = old[i].Placeholders, old[i].Real != real
		}
		if len(sec.Placeholders) == 0 || changed && keys.IsJWT(real) {
			sec.Placeholders = append([]string{keys.Placeholder(real)}, sec.Placeholders...)
		}
		out = append(out, sec)
	}
	return s.putLoginSecrets(a.Name, out)
}

// placeholdered is a login's files as boxes get them: each secret replaced by its
// newest placeholder.
func placeholdered(files []guestproto.File, secrets []keys.Secret) []guestproto.File {
	swaps := map[string]string{}
	for _, sec := range secrets {
		swaps[sec.Real] = sec.Placeholders[0]
	}
	return placeholderedBy(files, swaps)
}

// placeholderedBy is files with each real value in swaps replaced by its placeholder.
func placeholderedBy(files []guestproto.File, swaps map[string]string) []guestproto.File {
	out := make([]guestproto.File, len(files))
	for i, f := range files {
		f.Data = keys.Replace(slices.Clone(f.Data), swaps)
		out[i] = f
	}
	return out
}

// proxied reports whether a box can use its keys proxy: its hutch-guest gives
// every process the machine's environment. Older ones, in boxes from templates
// built before, get real logins until the template is rebuilt.
func (s *Server) proxied(ctx context.Context, w *workerConn, box string) bool {
	var v int
	if err := rpc.Call(ctx, w.sess, api.BoxGuestVersion, api.IDParams{ID: box}, &v); err != nil || v < 3 {
		log.Printf("keys: %s's hutch-guest predates the keys proxy, so it holds real logins; boxes from a rebuilt template don't", box)
		return false
	}
	return true
}

// boxFiles returns the stored credential files a box granted creds gets: logins
// and MCP keys as placeholders when it's proxied.
func (s *Server) boxFiles(creds []string, proxied bool) ([]guestproto.File, error) {
	stored, err := s.credentials()
	if err != nil {
		return nil, err
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	mcpLogins, real, err := s.mcpLogins(set)
	if err != nil {
		return nil, err
	}
	var files []guestproto.File
	for _, c := range stored {
		a, mcp, _ := strings.Cut(c.Part, "/")
		switch {
		case !granted(creds, a):
			continue
		case !proxied:
			files = append(files, c.Files...)
			continue
		case mcp != "":
			files = append(files, mcpFiles(a, c.Files, mcpLogins, real)...)
			continue
		}
		secrets, err := s.loginSecrets(a)
		if err != nil {
			return nil, err
		}
		if len(secrets) == 0 {
			// Placeholders come with the proxy's next table; until then the login
			// isn't written into boxes.
			log.Printf("keys: %s's login has no placeholders yet", a)
			continue
		}
		files = append(files, placeholdered(c.Files, secrets)...)
	}
	return files, nil
}

func (s *Server) keysTable() (keys.Table, error) {
	cert, key, err := s.keyCA()
	if err != nil {
		return keys.Table{}, err
	}
	set, err := s.adapters()
	if err != nil {
		return keys.Table{}, err
	}
	grants, err := s.store.allBoxCredentials()
	if err != nil {
		return keys.Table{}, err
	}
	reuse := time.Minute
	if c, err := s.settings(); err == nil {
		reuse = c.Duration("keys.refresh_reuse")
	}
	stored, err := s.credentials()
	if err != nil {
		return keys.Table{}, err
	}
	t := keys.Table{Grants: grants, CACert: cert, CAKey: key, Reuse: reuse}
	for _, a := range set.Sorted() {
		secrets, err := s.loginSecrets(a.Name)
		if err != nil {
			return t, err
		}
		// A login captured before the keys proxy gets its placeholders now.
		if i := slices.IndexFunc(stored, func(c api.Credentials) bool { return c.Part == a.Name }); len(secrets) == 0 && i >= 0 {
			if err := s.syncSecrets(a, stored[i].Files); err != nil {
				return t, err
			}
			if secrets, err = s.loginSecrets(a.Name); err != nil {
				return t, err
			}
		}
		if len(secrets) > 0 {
			t.Logins = append(t.Logins, keys.Login{Name: a.Name, Adapter: a.Name, Hosts: a.Hosts, Secrets: secrets})
		}
	}
	// MCP keys synced before the keys proxy get their placeholders now.
	for _, c := range stored {
		name, isMCP := strings.CutSuffix(c.Part, "/mcp")
		if a := set[name]; isMCP && a != nil {
			if have, err := s.mcpSecrets(name); err == nil && have == nil {
				if err := s.syncMCP(a, c.Files); err != nil {
					return t, err
				}
			}
		}
	}
	mcp, _, err := s.mcpLogins(set)
	if err != nil {
		return t, err
	}
	t.Logins = append(t.Logins, mcp...)
	return t, nil
}

// pushKeys sends the keys proxy's table to one worker, or with nil to all. Pushes
// are one at a time, so a worker never gets an older table after a newer one.
func (s *Server) pushKeys(ctx context.Context, only *workerConn) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	t, err := s.keysTable()
	if err != nil {
		log.Printf("keys: %v", err)
		return
	}
	s.mu.Lock()
	workers := slices.Collect(maps.Values(s.workers))
	s.mu.Unlock()
	for _, w := range workers {
		if only != nil && w != only {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := rpc.Call(ctx, w.sess, api.WorkerKeys, t, nil); err != nil {
			log.Printf("worker %s: keys: %v", w.id, err)
		}
		cancel()
	}
}

// keysRefreshed stores a login's new real values after the proxy refreshed it.
func (s *Server) keysRefreshed(ctx context.Context, p api.KeysRefreshedParams) error {
	if strings.Contains(p.Adapter, "/") {
		return fmt.Errorf("%s doesn't refresh", p.Adapter)
	}
	if err := s.storeRefresh(p); err != nil {
		return err
	}
	s.pushKeys(ctx, nil)
	// Boxes from older templates hold the real login, which this refresh replaced.
	go s.pushLogin(context.WithoutCancel(ctx), p.Adapter)
	return nil
}

func (s *Server) storeRefresh(p api.KeysRefreshedParams) error {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	secrets, err := s.loginSecrets(p.Adapter)
	if err != nil {
		return err
	}
	swaps := map[string]string{}
	for _, c := range p.Changes {
		i := slices.IndexFunc(secrets, func(x keys.Secret) bool { return x.Key == c.Key && (c.Old == "" || x.Real == c.Old) })
		if i < 0 {
			secrets = append(secrets, keys.Secret{Key: c.Key, Real: c.New, Placeholders: []string{c.Placeholder}})
			continue
		}
		if c.Old != "" {
			swaps[c.Old] = c.New
		}
		secrets[i].Real = c.New
		if !slices.Contains(secrets[i].Placeholders, c.Placeholder) {
			secrets[i].Placeholders = append([]string{c.Placeholder}, secrets[i].Placeholders...)
		}
	}
	part := credentialsPrefix + p.Adapter
	cur, err := s.store.layerPart(part)
	if err != nil {
		return err
	}
	var files []guestproto.File
	if err := json.Unmarshal(cur.content, &files); err != nil {
		return fmt.Errorf("%s's stored login: %w", p.Adapter, err)
	}
	for i := range files {
		files[i].Data = keys.Replace(files[i].Data, swaps)
	}
	data, _ := json.Marshal(files)
	if _, _, err := s.store.putLayerPart(part, -1, data); err != nil {
		return err
	}
	return s.putLoginSecrets(p.Adapter, secrets)
}

// boxKeys points a box at its keys proxy: the machine's CA in its trust store,
// and the proxy in every process's environment (guestproto.EnvPath).
func (s *Server) boxKeys(ctx context.Context, w *workerConn, box string) error {
	cert, _, err := s.keyCA()
	if err != nil {
		return err
	}
	proxy := fmt.Sprintf("http://%s:%d", keys.Gateway, keys.Port)
	local := "localhost,127.0.0.1,::1"
	env := strings.Join([]string{"HTTPS_PROXY=" + proxy, "https_proxy=" + proxy, "NO_PROXY=" + local, "no_proxy=" + local,
		"NODE_EXTRA_CA_CERTS=" + keys.CAPath}, "\n") + "\n"
	script := `set -e
mkdir -p "$(dirname "$3")" "$(dirname "$4")" /etc/ssl/certs
if [ "$(cat "$3" 2>/dev/null)" != "$1" ]; then
  printf '%s\n' "$1" > "$3"; chmod 644 "$3"
  if command -v update-ca-certificates >/dev/null; then update-ca-certificates >/dev/null 2>&1 || true; fi
  b=/etc/ssl/certs/ca-certificates.crt
  grep -qF "$(sed -n 2p "$3")" "$b" 2>/dev/null || cat "$3" >> "$b"
fi
printf '%s' "$2" > "$4"; chmod 644 "$4"`
	return s.run(ctx, w, box, api.ExecParams{User: "root", Argv: []string{"sh", "-c", script, "sh", strings.TrimSpace(string(cert)), env, keys.CAPath, guestproto.EnvPath}}, io.Discard, io.Discard)
}
