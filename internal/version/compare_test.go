package version

import "testing"

// An older CLI must not install its hutchd over a newer one.
func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.1.9", false},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0-rc.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0-rc.1", false},
		{"v0.1.0-rc.2", "v0.1.0-rc.10", true},
		{"v0.1.0-rc.10", "v0.1.0-rc.2", false},
		{"v0.1.0-alpha.2", "v0.1.0-alpha.beta", true},
		{"v0.1.0-alpha", "v0.1.0-alpha.1", true},
		{"v0.1.0-rc.2+build.10", "v0.1.0-rc.10+build.1", true},
		{"v0.1.0+build.1", "v0.1.0+build.2", false},
		{"v0.1.0-rc.01", "v0.1.0", false},
		{"v00.1.0", "v0.2.0", false},
		{"v0.1.0-", "v0.1.0", false},
		{"v0.10.0", "v0.9.0", false},
		{"dev", "v0.1.0", false},
		{"4a5fe90-dirty", "v0.1.0", false},
		{"v0.1.0", "v0.1.0-16-g4a5fe90", true},
		{"v0.1.0-16-g4a5fe90-dirty", "v0.1.0", false},
		{"v0.1.0-9-g4a5fe90", "v0.1.0-16-gbeef123", true},
		{"v0.1.0-16-g4a5fe90", "v0.1.0-9-gbeef123", false},
		{"v0.1.0-rc.1", "v0.1.0-3-g4a5fe90", true},
		{"v0.1.0-3-g4a5fe90", "v0.2.0", true},
		{"v0.2.0-rc.2-dirty", "v0.2.0-rc.3", true},
		{"v0.2.0-rc.3", "v0.2.0-rc.2-dirty", false},
		{"v0.2.0-rc.2", "v0.2.0-rc.2-dirty", true},
		{"v0.2.0-rc.2-5-g4a5fe90", "v0.2.0-rc.3", true},
		{"v0.2.0-rc.2", "v0.2.0-rc.2-5-g4a5fe90", true},
		{"v0.2.0-rc.2-5-g4a5fe90", "v0.2.0-rc.2-5-g4a5fe90-dirty", true},
		{"v0.2.0-rc.2-9-g4a5fe90", "v0.2.0-rc.2-10-gbeef123", true},
		{"v0.2.0-rc.3-dirty", "v0.2.0", true},
		{"v0.1.0", "v0.1.0-dirty", true},
		{"v0.1.0-dirty", "v0.1.0-1-g4a5fe90", true},
	} {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
