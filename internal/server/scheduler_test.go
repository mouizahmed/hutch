package server

import "testing"

// Placement decides what the fleet costs: elastic workers only when persistent ones
// are full, and the fullest worker first, so lightly used ones empty out and stop.
func TestChoose(t *testing.T) {
	box := need{memMiB: 4096, vcpus: 2, class: "intel"}
	cs := []candidate{
		{id: "cloud-empty", freeMiB: 28000, cpus: 8, diskOK: true, class: "intel", cloud: true},
		{id: "home-roomy", freeMiB: 20000, cpus: 8, diskOK: true, class: "intel"},
		{id: "home-full", freeMiB: 5000, cpus: 8, diskOK: true, class: "intel"},
		{id: "home-tight", freeMiB: 3000, cpus: 8, diskOK: true, class: "intel"},
		{id: "home-diskfull", freeMiB: 6000, cpus: 8, diskOK: false, class: "intel"},
	}
	pick := func(cs []candidate, n need) string {
		if i := choose(cs, n); i >= 0 {
			return cs[i].id
		}
		return ""
	}
	if got := pick(cs, box); got != "home-full" {
		t.Fatalf("got %s, want the fullest persistent worker that fits", got)
	}
	if got := pick(cs[:1], box); got != "cloud-empty" {
		t.Fatalf("got %s, want the elastic worker when nothing else fits", got)
	}

	// A fork needs its snapshot class; a template already on a worker beats fullness.
	fork := need{memMiB: 4096, vcpus: 2, class: "intel", strict: true, version: "app-v3"}
	cs = []candidate{
		{id: "amd", freeMiB: 5000, cpus: 8, diskOK: true, class: "amd"},
		{id: "intel-roomy", freeMiB: 20000, cpus: 8, diskOK: true, class: "intel", hasVersion: true},
		{id: "intel-full", freeMiB: 5000, cpus: 8, diskOK: true, class: "intel"},
	}
	if got := pick(cs, fork); got != "intel-roomy" {
		t.Fatalf("got %s, want the worker holding the template", got)
	}
	if got := pick(cs[:1], fork); got != "" {
		t.Fatalf("got %s, want no worker of another class for a fork", got)
	}
}
