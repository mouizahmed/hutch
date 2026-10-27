// Package conformance holds the provider contract: every provider passes the same
// tests, so the server's fleet code can rely on them alike.
package conformance

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/provider/aws"
	"github.com/mouizahmed/hutch/internal/provider/fake"
	"github.com/mouizahmed/hutch/internal/sigv4"
)

// run checks a node's lifecycle: provisioned nodes are listed with their tags and
// type, stop keeps the node, start brings it back, destroy removes it from the list.
func run(t *testing.T, p provider.Provider, nodeType string, wait func(id string, want provider.NodeState)) {
	ctx := context.Background()
	types, err := p.InstanceTypes(ctx)
	if err != nil || len(types) == 0 {
		t.Fatalf("instance types: %v %v", types, err)
	}
	for _, it := range types {
		if it.VCPUs == 0 || it.MemMiB == 0 || it.PerHour <= 0 || it.Arch == "" {
			t.Fatalf("incomplete instance type %+v", it)
		}
	}
	if _, err := p.Rates(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := p.ProvisionNode(ctx, provider.NodeSpec{Name: "hutch-conformance-" + time.Now().Format("150405"), Type: nodeType, DiskGB: 20,
		UserData: []byte(`{}`), Tags: map[string]string{provider.TagWorker: "conformance"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.DestroyNode(context.Background(), n.ID)
	find := func() *provider.Node {
		nodes, err := p.ListNodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range nodes {
			if x.ID == n.ID {
				return &x
			}
		}
		return nil
	}
	// Listings may lag a launch (EC2 does by seconds); within a minute is the contract.
	got := find()
	for i := 0; got == nil && i < 12; i++ {
		time.Sleep(5 * time.Second)
		got = find()
	}
	if got == nil || got.Type != nodeType || got.Tags[provider.TagWorker] != "conformance" || !got.Worker() {
		t.Fatalf("provisioned node listed as %+v", got)
	}
	wait(n.ID, provider.Running)
	if err := p.StopNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	wait(n.ID, provider.Stopped)
	if _, err := p.StartNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	wait(n.ID, provider.Running)
	if err := p.DestroyNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	gone := find() == nil
	for i := 0; !gone && i < 24; i++ {
		time.Sleep(5 * time.Second)
		gone = find() == nil
	}
	if !gone {
		t.Fatal("destroyed node still listed")
	}
}

func TestFake(t *testing.T) {
	p := fake.New()
	run(t, p, "w.large", func(id string, want provider.NodeState) {
		nodes, _ := p.ListNodes(context.Background())
		for _, n := range nodes {
			if n.ID == id && n.State != want {
				t.Fatalf("%s is %s, want %s", id, n.State, want)
			}
		}
	})
}

// TestAWS launches and terminates a real instance: set HUTCH_AWS_CONFORMANCE=1,
// with AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and the HUTCH_AWS_IMAGE,
// HUTCH_AWS_SUBNET, and HUTCH_AWS_GROUP to launch into.
func TestAWS(t *testing.T) {
	if os.Getenv("HUTCH_AWS_CONFORMANCE") == "" {
		t.Skip("launches a real instance; set HUTCH_AWS_CONFORMANCE=1")
	}
	p, err := aws.New(aws.Config{Region: "us-east-1", Fleet: "conformance",
		Credentials: sigv4.Credentials{AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY")},
		ImageID:     os.Getenv("HUTCH_AWS_IMAGE"), SubnetID: os.Getenv("HUTCH_AWS_SUBNET"), SecurityGroupID: os.Getenv("HUTCH_AWS_GROUP")})
	if err != nil {
		t.Fatal(err)
	}
	run(t, p, "c8i.large", func(id string, want provider.NodeState) {
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			n, err := p.Node(context.Background(), id)
			if err == nil && n.State == want {
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatalf("%s didn't become %s", id, want)
	})
}
