// Package provider supplies workers from a cloud. Providers only create, start,
// stop, and destroy machines and report prices; Hutch owns everything above that.
package provider

import (
	"context"
	"errors"
	"time"
)

var ErrUnsupported = errors.New("not supported by this provider")

type Provider interface {
	Name() string
	// InstanceTypes lists the types that can run workers (KVM), with prices.
	InstanceTypes(ctx context.Context) ([]InstanceType, error)
	// ProvisionNode starts a new worker from the worker image; its user data carries
	// only the join token.
	ProvisionNode(ctx context.Context, spec NodeSpec) (*Node, error)
	// StopNode keeps the disk and stops compute billing.
	StopNode(ctx context.Context, id string) error
	StartNode(ctx context.Context, id string) (*Node, error)
	DestroyNode(ctx context.Context, id string) error
	// ListNodes lists this fleet's nodes that aren't destroyed.
	ListNodes(ctx context.Context) ([]Node, error)
	Rates(ctx context.Context) (Rates, error)
	// BilledCosts returns what the provider billed for this fleet on a day (UTC),
	// or ErrUnsupported.
	BilledCosts(ctx context.Context, day time.Time) ([]CostLine, error)
}

type InstanceType struct {
	Name    string  `json:"name"`
	VCPUs   int     `json:"vcpus"`
	MemMiB  int     `json:"mem_mib"`
	Arch    string  `json:"arch"`   // amd64, arm64
	Vendor  string  `json:"vendor"` // intel, amd, graviton
	PerHour float64 `json:"per_hour"`
}

type NodeSpec struct {
	Name     string
	Type     string
	DiskGB   int
	UserData []byte
	// Tags go on the node and everything created with it.
	Tags map[string]string
}

type NodeState string

const (
	Pending  NodeState = "pending"
	Running  NodeState = "running"
	Stopping NodeState = "stopping"
	Stopped  NodeState = "stopped"
)

type Node struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	State     NodeState         `json:"state"`
	PrivateIP string            `json:"private_ip,omitempty"`
	Launched  time.Time         `json:"launched"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// Rates are the prices beyond instance hours, for cost estimates.
type Rates struct {
	DiskGBMonth   float64 `json:"disk_gb_month"`   // worker and server disks
	IPv4Hour      float64 `json:"ipv4_hour"`       // per public IPv4 address
	BucketGBMonth float64 `json:"bucket_gb_month"` // bucket storage
	PutPer1000    float64 `json:"put_per_1000"`
	GetPer1000    float64 `json:"get_per_1000"`
	EgressGB      float64 `json:"egress_gb"` // transfer out to the internet
}

// CostLine is one billed amount, attributed by the tags Hutch puts on resources.
type CostLine struct {
	Day      time.Time `json:"day"`
	Category string    `json:"category"` // compute, storage, transfer, other
	Box      string    `json:"box,omitempty"`
	Worker   string    `json:"worker,omitempty"`
	USD      float64   `json:"usd"`
}

// Tags Hutch puts on everything it creates.
const (
	TagFleet  = "hutch:fleet"
	TagWorker = "hutch:worker"
	TagBox    = "hutch:box"
)
