// Package fake is an in-memory provider for tests.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/provider"
)

type Provider struct {
	mu    sync.Mutex
	nodes map[string]*provider.Node
	n     int
	Types []provider.InstanceType
	// Calls records every change, like "stop i-1".
	Calls []string
}

func New() *Provider {
	return &Provider{nodes: map[string]*provider.Node{},
		Types: []provider.InstanceType{{Name: "w.large", VCPUs: 2, MemMiB: 8192, Arch: "amd64", Vendor: "intel", PerHour: 0.1}}}
}

func (p *Provider) Name() string { return "fake" }

func (p *Provider) InstanceTypes(context.Context) ([]provider.InstanceType, error) {
	return p.Types, nil
}

func (p *Provider) ProvisionNode(_ context.Context, spec provider.NodeSpec) (*provider.Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	n := &provider.Node{ID: fmt.Sprintf("i-%d", p.n), Name: spec.Name, Type: spec.Type, State: provider.Running, Launched: time.Now(), Tags: spec.Tags}
	p.nodes[n.ID] = n
	p.Calls = append(p.Calls, "provision "+n.ID)
	c := *n
	return &c, nil
}

// Add puts a node in a state, as if it had been provisioned earlier.
func (p *Provider) Add(id, typ string, state provider.NodeState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[id] = &provider.Node{ID: id, Type: typ, State: state}
}

func (p *Provider) set(op, id string, state provider.NodeState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.nodes[id]
	if !ok {
		return fmt.Errorf("no node %s", id)
	}
	p.Calls = append(p.Calls, op+" "+id)
	if state == "" {
		delete(p.nodes, id)
	} else {
		n.State = state
	}
	return nil
}

func (p *Provider) StopNode(_ context.Context, id string) error {
	return p.set("stop", id, provider.Stopped)
}

func (p *Provider) StartNode(_ context.Context, id string) (*provider.Node, error) {
	if err := p.set("start", id, provider.Running); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := *p.nodes[id]
	return &c, nil
}

func (p *Provider) DestroyNode(_ context.Context, id string) error { return p.set("destroy", id, "") }

func (p *Provider) ListNodes(context.Context) ([]provider.Node, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []provider.Node
	for _, n := range p.nodes {
		out = append(out, *n)
	}
	return out, nil
}

func (p *Provider) Rates(context.Context) (provider.Rates, error) {
	return provider.Rates{DiskGBMonth: 0.08, IPv4Hour: 0.005, BucketGBMonth: 0.023, PutPer1000: 0.005, GetPer1000: 0.0004}, nil
}

func (p *Provider) BilledCosts(context.Context, time.Time) ([]provider.CostLine, error) {
	return nil, provider.ErrUnsupported
}
