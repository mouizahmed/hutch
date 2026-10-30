package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/yamux"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func TestMachineVersionHandshake(t *testing.T) {
	for _, tt := range []struct {
		name     string
		protocol int
		missing  bool
		refused  bool
		want     string
	}{
		{name: "same protocol", protocol: api.Protocol},
		{name: "newer compatible machine", protocol: api.Protocol + 1},
		{name: "older machine", protocol: api.Protocol - 1, want: "hutch machine upgrade"},
		{name: "machine predates hello", missing: true, want: "hutch machine upgrade"},
		{name: "machine refuses old client", refused: true, want: "upgrade hutch on this computer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b := net.Pipe()
			cfg := yamux.DefaultConfig()
			cfg.LogOutput = nil
			cfg.Logger = log.New(io.Discard, "", 0)
			machine, err := yamux.Server(a, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer machine.Close()
			client, err := yamux.Client(b, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			mux := rpc.Mux{}
			if !tt.missing {
				mux[api.Hello] = func(_ context.Context, data json.RawMessage, _ *rpc.Out) (any, error) {
					var request api.Handshake
					if err := json.Unmarshal(data, &request); err != nil {
						return nil, err
					}
					if request.Protocol != api.Protocol {
						return nil, errors.New("client sent the wrong protocol")
					}
					if tt.refused {
						return nil, errors.New("upgrade hutch on this computer")
					}
					return api.Handshake{Protocol: tt.protocol, Version: "fixture"}, nil
				}
			}
			go rpc.Serve(t.Context(), machine, mux, nil)
			err = checkVersion(t.Context(), client)
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("got %v; want %q", err, tt.want)
			}
		})
	}
}
