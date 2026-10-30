package keys

import (
	"slices"
	"testing"
)

func TestMCPTransportTargets(t *testing.T) {
	for _, tt := range []struct {
		url        string
		configured []string
		want       []string
	}{
		{"https://mcp.example/mcp", nil, []string{"mcp.example"}},
		{"https://mcp.example:443/mcp", nil, []string{"mcp.example"}},
		{"https://mcp.example:8443/mcp", nil, []string{"mcp.example:8443"}},
		{"http://127.0.0.1:3000/mcp", nil, nil},
		{"http://mcp.example/mcp", []string{"mcp.example"}, nil},
		{"", []string{"api.example:8443"}, []string{"api.example:8443"}},
		{"", nil, nil},
	} {
		if got := MCPHosts(tt.url, tt.configured); !slices.Equal(got, tt.want) {
			t.Errorf("%q: got %v; want %v", tt.url, got, tt.want)
		}
	}
}

func TestCredentialPortBoundary(t *testing.T) {
	l := Login{Hosts: []string{"api.example", "mcp.example:8443"}}
	for _, tt := range []struct {
		host, port string
		want       bool
	}{
		{"api.example", "443", true},
		{"api.example", "8443", false},
		{"sub.api.example", "443", true},
		{"api.example.evil", "443", false},
		{"mcp.example", "8443", true},
		{"mcp.example", "443", false},
		{"mcp.example", "80", false},
	} {
		if got := l.ForTarget(tt.host, tt.port); got != tt.want {
			t.Errorf("%s:%s: got %v", tt.host, tt.port, got)
		}
	}
}
