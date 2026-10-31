package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
)

// doctorUser checks captured configuration without waking boxes, starting an
// agent, or contacting a provider. Its output never includes credential values.
func (s *Server) doctorUser(defaultAgent string) []api.Check {
	// Login capture and token refresh update files and placeholders together.
	// Do not diagnose a mismatch while that update is still in progress.
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	var checks []api.Check
	add := func(name, status, detail, fix string) {
		checks = append(checks, api.Check{Area: "user", Name: name, Status: status, Detail: detail, Fix: fix})
	}
	layer, _, err := s.userLayer()
	if err != nil {
		add("config", "fail", "stored user layer could not be read", "hutch user sync")
		return checks
	}
	set, err := agents.Load(layer.Adapters)
	if err != nil {
		add("adapters", "fail", "stored adapters could not be loaded", "check ~/.config/hutch/agents, then hutch user sync")
		return checks
	}
	if a := set[defaultAgent]; a == nil || !a.Agent() {
		add("default agent", "fail", fmt.Sprintf("%q does not name an agent adapter", defaultAgent), "hutch config set default_agent <agent>; hutch user sync for custom adapters")
	} else {
		add("default agent", "ok", defaultAgent, "")
	}
	stored, err := s.credentials()
	if err != nil {
		add("logins", "fail", "captured credentials could not be read", "capture the logins again with hutch login <agent>")
		return checks
	}
	logins := map[string][]guestproto.File{}
	for _, c := range stored {
		if strings.Contains(c.Part, "/") {
			continue // MCP tokens are checked separately.
		}
		if set[c.Part] == nil {
			add("login "+c.Part, "warn", "captured login has no matching adapter", "restore its custom adapter and run hutch user sync")
			continue
		}
		logins[c.Part] = c.Files
	}
	for _, a := range set.Sorted() {
		files, exists := logins[a.Name]
		if !exists && a.Name != defaultAgent {
			continue // Other built-in agents and GitHub are optional.
		}
		if !exists {
			if len(a.Credentials) == 0 {
				add("login "+a.Name, "ok", "adapter does not declare a Hutch-managed login", "")
			} else {
				add("login "+a.Name, "warn", "no login captured for the default agent", "hutch login "+a.Name+" (or configure the agent's own authentication)")
			}
			continue
		}
		secrets, err := s.loginSecrets(a.Name)
		if err != nil {
			add("login "+a.Name, "fail", "stored proxy credentials could not be read", "hutch login "+a.Name)
			continue
		}
		checks = append(checks, capturedLoginCheck(a, files, secrets))
	}
	proxied, real, err := s.mcpLogins(set)
	if err != nil {
		add("MCP keys", "fail", "stored MCP credentials could not be read", "hutch user sync")
	} else if len(real) > 0 {
		add("MCP keys", "warn", fmt.Sprintf("%d credential value(s) cannot use HTTPS header substitution and remain real in granted boxes", len(real)), "declare mcp_hosts.<server> for header tokens; plain HTTP and native OAuth client secrets require native authentication")
	} else if len(proxied) > 0 {
		add("MCP keys", "ok", fmt.Sprintf("%d server(s) have HTTPS targets for token substitution", len(proxied)), "")
	}
	for _, a := range set.Sorted() {
		if a.MCP == nil {
			continue
		}
		for _, f := range layer.Files[a.Name] {
			if f.Path == a.MCP.File && len(agents.MCPServers(f.Path, f.Data, a.MCP.Key)) > 0 {
				add("MCP readiness", "warn", "configured servers have not been started by this offline check; native-client OAuth is not validated", "hutch doctor --template for runtime and transport checks; verify authentication in the native client")
				return checks
			}
		}
	}
	return checks
}

func capturedLoginCheck(a *agents.Adapter, files []guestproto.File, secrets []keys.Secret) api.Check {
	c := api.Check{Area: "user", Name: "login " + a.Name, Status: "ok", Detail: "captured token fields and proxy placeholders match; provider validity not checked"}
	found, err := loginValues(a, files)
	if err != nil || len(found) == 0 {
		c.Status, c.Detail, c.Fix = "warn", "captured files contain none of the adapter's declared token fields", "hutch login "+a.Name
		return c
	}
	for real, key := range found {
		if !slices.ContainsFunc(secrets, func(sec keys.Secret) bool {
			return sec.Real == real && keys.Norm(sec.Key) == key && len(sec.Placeholders) > 0 && sec.Placeholders[0] != "" && sec.Placeholders[0] != real
		}) {
			c.Status, c.Detail, c.Fix = "fail", "captured token fields do not have matching proxy placeholders", "hutch login "+a.Name
			return c
		}
	}
	return c
}
