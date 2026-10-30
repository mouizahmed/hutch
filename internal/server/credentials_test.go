package server

import "testing"

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
