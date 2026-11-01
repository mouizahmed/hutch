//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/worker"
)

// ServeCLI answers the CLI on a worker's socket. While the lead is reachable it
// refuses, naming the lead: a worker never passes calls on to it, so a worker
// taken over can't act with the lead's authority. While the lead is down it
// serves this machine's own boxes.
func (a *Agent) ServeCLI(ctx context.Context, socket string, dir link.Dir) error {
	host := dir.Lead()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	guard := func() error {
		if a.server.Load() != nil {
			return fmt.Errorf("this machine is a worker; its lead is %s: point hutch at the lead (hutch config set machine <its SSH host>)", host)
		}
		return nil
	}
	down := fmt.Sprintf("the lead (%s) is unreachable: this machine serves only its own boxes (ls, attach, shell, exec, console, forward, sleep, wake) until it's back", host)

	mux := rpc.Mux{
		api.Hello: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			if err := guard(); err != nil {
				return nil, err
			}
			return api.Handshake{Protocol: api.Protocol, Version: version.String(), LeadDown: down}, nil
		},
		api.BoxList: func(context.Context, json.RawMessage, *rpc.Out) (any, error) {
			if err := guard(); err != nil {
				return nil, err
			}
			bs, err := a.w.List()
			if err != nil {
				return nil, err
			}
			out := []api.Box{}
			for _, b := range bs {
				ab := toAPI(a.w, b)
				if b.Store != nil && strings.Contains(b.Store.Prefix, "/templates/") {
					ab.Kind = api.BoxVersion
				}
				out = append(out, ab)
			}
			return out, nil
		},
	}
	for _, m := range []string{api.BoxConsole, api.BoxStatus, api.BoxSleep, api.BoxWake} {
		h := a.mux[m]
		mux[m] = func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			if err := guard(); err != nil {
				return nil, err
			}
			return h(ctx, params, out)
		}
	}
	exec := a.mux[api.BoxExec]
	mux[api.BoxExec] = func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
		if err := a.awake(ctx, params, guard); err != nil {
			return nil, err
		}
		return exec(ctx, params, out)
	}
	streams := rpc.Streams{}
	for _, m := range []string{api.BoxPTY, api.BoxExecIn, api.BoxDial, api.BoxSerial} {
		h, wake := a.streams[m], m != api.BoxSerial
		streams[m] = func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			if err := guard(); err != nil {
				return err
			}
			if wake {
				if err := a.awake(ctx, params, guard); err != nil {
					return err
				}
			}
			return h(ctx, params, conn)
		}
	}
	return rpc.ServeSocket(ctx, socket, mux, streams)
}

// awake wakes the box a call names, as the lead does before a session.
func (a *Agent) awake(ctx context.Context, params json.RawMessage, guard func() error) error {
	if err := guard(); err != nil {
		return err
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := rpc.Decode(params, &p); err != nil {
		return err
	}
	b, err := a.w.Get(p.ID)
	if err != nil {
		return err
	}
	if b.State == worker.Asleep || b.State == worker.Archived {
		if _, err := a.w.Wake(ctx, p.ID); err != nil {
			return fmt.Errorf("wake %s: %w", p.ID, err)
		}
	}
	return nil
}
