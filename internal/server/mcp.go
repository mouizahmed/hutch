package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
)

// MCP servers' keys go through the keys proxy like logins: each server's keys
// (the values in its secret fields that look like keys) get placeholders, kept
// in keyproxy/<adapter>/mcp by server name. A server's keys are swapped toward
// its hosts: its URL's host, plus mcp_hosts.<server>. One with no hosts (a server
// in the box whose hosts aren't named) keeps its real keys.

func (s *Server) mcpSecrets(adapter string) (map[string][]keys.Secret, error) {
	p, err := s.store.layerPart(keysPrefix + adapter + "/mcp")
	if err != nil || len(p.content) == 0 {
		return nil, err
	}
	var out map[string][]keys.Secret
	return out, json.Unmarshal(p.content, &out)
}

// syncMCP finds the keys in an adapter's MCP secret files, keeping the
// placeholders of keys it had before.
func (s *Server) syncMCP(a *agents.Adapter, files []guestproto.File) error {
	if a.MCP == nil {
		return nil
	}
	old, err := s.mcpSecrets(a.Name)
	if err != nil {
		return err
	}
	out := map[string][]keys.Secret{}
	for _, f := range files {
		if f.Path != a.MCP.File {
			continue
		}
		for name, server := range agents.MCPServers(f.Path, f.Data, a.MCP.Key) {
			for k, v := range agents.MCPSecretValues(server, a.MCP.Secrets) {
				if !keys.LooksSecret(v) {
					continue
				}
				sec := keys.Secret{Key: k, Real: v}
				if i := slices.IndexFunc(old[name], func(o keys.Secret) bool { return o.Key == k }); i >= 0 {
					sec.Placeholders = slices.Clone(old[name][i].Placeholders)
					if old[name][i].Real != v && keys.IsJWT(v) {
						sec.Placeholders = append([]string{keys.Placeholder(v)}, sec.Placeholders...)
					}
				}
				if len(sec.Placeholders) == 0 {
					sec.Placeholders = []string{keys.Placeholder(v)}
				}
				out[name] = append(out[name], sec)
			}
		}
	}
	data, _ := json.Marshal(out)
	if _, _, err = s.store.putLayerPart(keysPrefix+a.Name+"/mcp", -1, data); err != nil {
		return err
	}
	// Last, so a sync cut short is found stale and run again.
	_, _, err = s.store.putLayerPart(keysPrefix+a.Name+"/mcp-source", -1, []byte(filesDigest(files)))
	return err
}

func filesDigest(files []guestproto.File) string {
	data, _ := json.Marshal(files)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// resyncMCP re-reads MCP keys whose record doesn't match the stored secret
// files, as after a sync cut short: otherwise new keys would go into boxes
// real. It reports whether any changed.
func (s *Server) resyncMCP(set agents.Set) (bool, error) {
	stored, err := s.credentials()
	if err != nil {
		return false, err
	}
	changed := false
	for _, c := range stored {
		name, isMCP := strings.CutSuffix(c.Part, "/mcp")
		a := set[name]
		if !isMCP || a == nil {
			continue
		}
		src, err := s.store.layerPart(keysPrefix + name + "/mcp-source")
		if err != nil {
			return changed, err
		}
		if string(src.content) == filesDigest(c.Files) {
			continue
		}
		if err := s.syncMCP(a, c.Files); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// mcpLogins returns the MCP servers whose keys the proxy swaps, as logins, and
// the real keys that stay real in boxes: those of servers with no hosts.
func (s *Server) mcpLogins(set agents.Set) ([]keys.Login, map[string]bool, error) {
	l, _, err := s.userLayer()
	if err != nil {
		return nil, nil, err
	}
	named := map[string][]string{}
	if c, err := s.settings(); err == nil {
		for server, st := range c.Named("mcp_hosts.*") {
			named[server], _ = st.Value.([]string)
		}
	}
	var logins []keys.Login
	real := map[string]bool{}
	for _, a := range set.Sorted() {
		if a.MCP == nil {
			continue
		}
		if len(a.MCP.RawSecrets) > 0 {
			part, err := s.store.layerPart(credentialsPrefix + a.Name + "/mcp")
			if err != nil {
				return nil, nil, err
			}
			if len(part.content) > 0 {
				var files []guestproto.File
				if err := json.Unmarshal(part.content, &files); err != nil {
					return nil, nil, err
				}
				for _, f := range files {
					if f.Path != a.MCP.File {
						continue
					}
					for _, server := range agents.MCPServers(f.Path, f.Data, a.MCP.Key) {
						for _, value := range agents.MCPSecretValues(server, a.MCP.RawSecrets) {
							real[value] = true
						}
					}
				}
			}
		}
		secrets, err := s.mcpSecrets(a.Name)
		if err != nil {
			return nil, nil, err
		}
		urls := map[string]string{}
		for _, f := range l.Files[a.Name] {
			if f.Path == a.MCP.File {
				for name, server := range agents.MCPServers(f.Path, f.Data, a.MCP.Key) {
					urls[name], _ = server["url"].(string)
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			hosts := keys.MCPHosts(urls[name], named[name])
			if len(hosts) == 0 {
				for _, sec := range secrets[name] {
					real[sec.Real] = true
				}
				continue
			}
			var headers []string
			for _, sec := range secrets[name] {
				if _, header, ok := strings.Cut(sec.Key, "."); ok {
					headers = append(headers, header)
				}
			}
			logins = append(logins, keys.Login{Name: a.Name + "/mcp/" + name, Adapter: a.Name, Hosts: hosts, Headers: headers, Secrets: secrets[name]})
		}
	}
	return logins, real, nil
}

// mcpFiles is an adapter's MCP secret files as a box gets them: keys of servers
// with hosts as placeholders.
func mcpFiles(adapter string, files []guestproto.File, logins []keys.Login, real map[string]bool) ([]guestproto.File, error) {
	swaps := map[string]string{}
	for _, l := range logins {
		if l.Adapter != adapter {
			continue
		}
		for _, sec := range l.Secrets {
			if !real[sec.Real] {
				swaps[sec.Real] = sec.Placeholders[0]
			}
		}
	}
	return placeholderedBy(files, swaps)
}
