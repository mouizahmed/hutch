package durable

import (
	"strings"
	"testing"
)

func TestRecoveryRejectsZeroThreads(t *testing.T) {
	r := Recovery{KDF: "argon2id", Time: 1, Memory: 8, Salt: make([]byte, 16)}
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("malformed recovery parameters panicked: %v", p)
		}
	}()
	if _, err := r.UnwrapBytes("fixture", "passphrase"); err == nil {
		t.Fatal("invalid recovery settings accepted")
	}
}

func TestRecoveryParameterBounds(t *testing.T) {
	valid := Recovery{KDF: "argon2id", Time: 3, Memory: 256 << 10, Threads: 4, Salt: make([]byte, 16)}
	if err := valid.validate(); err != nil {
		t.Fatalf("existing Hutch recovery parameters rejected: %v", err)
	}
	for _, tt := range []struct {
		name   string
		change func(*Recovery)
	}{
		{"unknown KDF", func(r *Recovery) { r.KDF = "unknown" }},
		{"zero time", func(r *Recovery) { r.Time = 0 }},
		{"excessive time", func(r *Recovery) { r.Time = ^uint32(0) }},
		{"zero threads", func(r *Recovery) { r.Threads = 0 }},
		{"excessive threads", func(r *Recovery) { r.Threads = 255 }},
		{"insufficient memory", func(r *Recovery) { r.Memory = 31 }},
		{"excessive memory", func(r *Recovery) { r.Memory = ^uint32(0) }},
		{"missing salt", func(r *Recovery) { r.Salt = nil }},
		{"oversized salt", func(r *Recovery) { r.Salt = make([]byte, 65) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.change(&r)
			// Validate before calling Argon2: a regression must not allocate
			// several terabytes merely to test an excessive-memory header.
			if err := r.validate(); err == nil {
				t.Fatal("unsafe recovery parameters accepted")
			}
		})
	}
}

func TestRecoveryRoundTrip(t *testing.T) {
	key := NewKey()
	r := Wrap(key, "fixture passphrase")
	got, err := r.Unwrap("fixture passphrase")
	if err != nil || got != key {
		t.Fatal("existing recovery format failed to round trip")
	}
	if _, err := r.Unwrap("wrong passphrase"); err == nil || !strings.Contains(err.Error(), "wrong recovery passphrase") {
		t.Fatal("wrong recovery passphrase was accepted")
	}
}
