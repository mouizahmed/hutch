package keys

import (
	"net"
	"net/url"
	"slices"
	"strings"
)

// MCPHosts is the set of HTTPS targets where an MCP's keys can be substituted.
// Plain HTTP does not use the CONNECT proxy, so it must keep real credentials.
// A stdio server has no URL and requires the user's declared HTTPS targets.
func MCPHosts(rawURL string, configured []string) []string {
	hosts := slices.Clone(configured)
	if rawURL == "" {
		return hosts
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	if !slices.Contains(hosts, host) {
		hosts = append(hosts, host)
	}
	return hosts
}
