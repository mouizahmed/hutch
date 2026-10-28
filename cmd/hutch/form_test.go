package main

import "testing"

// A VM machine gets the largest size of which three fit; a gVisor machine, where a
// size is a cap, the largest within half its memory.
func TestSuggestSize(t *testing.T) {
	def := boxSize{4, 8192}
	cases := []struct {
		name    string
		targets []fitTarget
		want    boxSize
	}{
		{"no machine", nil, def},
		{"8 GB VPS, gVisor", []fitTarget{{"vps", 4, 7775, true}}, boxSize{2, 4096}},
		{"64 GB server, VMs", []fitTarget{{"ax", 16, 65536, false}}, boxSize{8, 16384}},
		{"16 GB, VMs", []fitTarget{{"dev", 4, 15872, false}}, boxSize{2, 4096}},
		{"32 GB, gVisor", []fitTarget{{"big", 8, 32768, true}}, boxSize{8, 16384}},
	}
	for _, c := range cases {
		if got := suggestSize(c.targets, 0.1, def); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
