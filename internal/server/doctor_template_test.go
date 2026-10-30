package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestFingerprintComparison(t *testing.T) {
	for _, data := range []string{"", "null", "{}", `{"working files":"short"}`} {
		if _, err := decodeFingerprint([]byte(data)); err == nil {
			t.Fatalf("invalid fingerprint accepted: %q", data)
		}
	}
	first, err := decodeFingerprint([]byte(`{"working files":"` + strings.Repeat("a", 64) + `","tool node":"before"}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := decodeFingerprint([]byte(`{"tool node":"before","working files":"` + strings.Repeat("a", 64) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if changes := changedFingerprints(first, next); len(changes) != 0 {
		t.Fatalf("JSON order changed fingerprint: %v", changes)
	}
	next["tool node"] = "after"
	next["tool go"] = "new"
	delete(next, "working files")
	want := []string{"tool go", "tool node", "working files"}
	if got := changedFingerprints(first, next); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes=%v; want %v", got, want)
	}
}
