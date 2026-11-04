package main

import "testing"

// A machine is named as it's reached, without the user or port; an IP address
// names nothing, so its hostname is used instead.
func TestMachineName(t *testing.T) {
	for in, want := range map[string]string{
		"hutch-vps2": "hutch-vps2", "ubuntu@hutch-vps2": "hutch-vps2", "ssh://ubuntu@box.example.com:2222": "box.example.com",
		"203.0.113.7": "", "ubuntu@203.0.113.7": "", "[2001:db8::1]:22": "",
	} {
		if got := machineName(in); got != want {
			t.Errorf("machineName(%q) = %q, want %q", in, got, want)
		}
	}
}
