package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/provider/aws"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// fleet is the server's cloud provider: where workers come from when the ones it
// has are full.
type fleet struct {
	p   provider.Provider
	cfg fleetConfig
	// grow serializes starting and provisioning, so one shortage starts one worker.
	grow sync.Mutex

	typesMu sync.Mutex
	types   []provider.InstanceType
	typesAt time.Time

	pricesMu sync.Mutex
	price    prices
	nodesMu  sync.Mutex
	nodes    []provider.Node // workers, as last listed
	server   *provider.Node
}

// fleetConfig is provider.json in the data directory.
type fleetConfig struct {
	AWS *aws.Config `json:"aws"`
	// Address is the host:port workers dial: the server's private address.
	Address string `json:"address"`
	// RootGB is the worker image's root volume, billed with each worker's data disk.
	RootGB int `json:"root_gb"`
	// ServerDiskGB is the server instance's disk, when hutch server init created it.
	ServerDiskGB int `json:"server_disk_gb,omitempty"`
}

func loadFleet(dir string) (*fleet, error) {
	data, err := os.ReadFile(filepath.Join(dir, "provider.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg fleetConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("provider.json: %w", err)
	}
	return newFleet(cfg)
}

func newFleet(cfg fleetConfig) (*fleet, error) {
	if cfg.AWS == nil {
		return nil, errors.New("provider.json names no provider")
	}
	p, err := aws.New(*cfg.AWS)
	if err != nil {
		return nil, err
	}
	return &fleet{p: p, cfg: cfg}, nil
}

// instanceTypes returns the provider's types, refreshed daily.
func (f *fleet) instanceTypes(ctx context.Context) ([]provider.InstanceType, error) {
	f.typesMu.Lock()
	defer f.typesMu.Unlock()
	if f.types != nil && time.Since(f.typesAt) < 24*time.Hour {
		return f.types, nil
	}
	types, err := f.p.InstanceTypes(ctx)
	if err != nil {
		return f.types, err
	}
	f.types, f.typesAt = types, time.Now()
	return types, nil
}

func (f *fleet) instanceType(ctx context.Context, name string) (provider.InstanceType, error) {
	types, err := f.instanceTypes(ctx)
	if err != nil {
		return provider.InstanceType{}, err
	}
	for _, t := range types {
		if t.Name == name {
			return t, nil
		}
	}
	return provider.InstanceType{}, fmt.Errorf("%s isn't an instance type that can run workers", name)
}

// limitError means a limit stops a box from starting now; it waits.
type limitError struct {
	key   string
	value int64
}

func (e *limitError) Error() string { return fmt.Sprintf("%s (%d) reached", e.key, e.value) }

// limits resolves a limits.* key: yours, capped by the server's when it sets one.
func (s *Server) limits(key string) (any, error) {
	cfg, err := s.settings()
	if err != nil {
		return nil, err
	}
	v, err := cfg.Get(key)
	if err != nil {
		return nil, err
	}
	d, err := s.store.config("server")
	if err != nil {
		return nil, err
	}
	doc, err := config.Parse(config.Server, "server config", []byte(d.Content))
	if err != nil {
		return nil, err
	}
	sv, err := config.New(doc).Get(key)
	if err != nil || sv.Layer != config.Server {
		return v.Value, err
	}
	switch a := v.Value.(type) {
	case int64:
		return min(a, sv.Value.(int64)), nil
	case time.Duration:
		return min(a, sv.Value.(time.Duration)), nil
	}
	return v.Value, nil
}

func (s *Server) limitInt(key string) int64 {
	v, err := s.limits(key)
	if err != nil {
		return 0
	}
	return v.(int64)
}

// placeWait places a box, starting an elastic worker when none has room, and waiting
// (queued, with a notification) while a limit is reached or persistent workers are
// full. It returns when the box can start, or ctx ends.
func (s *Server) placeWait(ctx context.Context, n need, requested, box string, out io.Writer) (*workerConn, func(), error) {
	reason := ""
	for {
		lim, err := s.spendReached()
		if err != nil {
			return nil, nil, err
		}
		var w *workerConn
		var release func()
		if lim == nil {
			if w, release, err = s.place(n, requested); err == nil {
				return w, release, nil
			}
		} else {
			err = lim
		}
		if requested != "" || (!errors.Is(err, errNoRoom) && !errors.As(err, &lim)) {
			return nil, nil, err
		}
		if f := s.fleet.Load(); f != nil && lim == nil {
			gerr := s.grow(ctx, f, n, out)
			if gerr == nil {
				continue
			}
			if !errors.As(gerr, &lim) {
				return nil, nil, gerr
			}
			err = gerr
		}
		if err.Error() != reason {
			if reason == "" {
				s.notify("queued", box, "Box queued", fmt.Sprintf("%s is queued: %v", box, err))
			}
			reason = err.Error()
			fmt.Fprintf(out, "Queued: %v. %s starts when there's room (Ctrl-C to give up).\n", err, box)
			s.store.event(box, "queued", reason)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

var errNoRoom = errors.New("no worker has room")

// grow starts a stopped elastic worker, or provisions a new one, and waits for it to
// connect.
func (s *Server) grow(ctx context.Context, f *fleet, n need, out io.Writer) error {
	f.grow.Lock()
	defer f.grow.Unlock()
	// Another call may have made room while this one waited for the lock.
	if _, release, err := s.place(n, ""); err == nil {
		release()
		return nil
	}
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	wtype := cfg.String("providers.aws.worker_type")
	t, err := f.instanceType(ctx, wtype)
	if err != nil {
		return err
	}
	margin := cfg.Percent("worker.memory_margin") / 100
	if int(float64(t.MemMiB)*(1-margin)) < n.memMiB || t.VCPUs < n.vcpus {
		return fmt.Errorf("a %s box doesn't fit a %s worker (%d vCPUs, %s); set providers.aws.worker_type larger", n, wtype, t.VCPUs, mib(t.MemMiB))
	}
	nodes, err := f.listNodes(ctx)
	if err != nil {
		return err
	}
	running := 0
	var stopped []provider.Node
	for _, node := range nodes {
		switch node.State {
		case provider.Pending, provider.Running:
			running++
		case provider.Stopped:
			if node.Type == wtype {
				stopped = append(stopped, node)
			}
		}
	}
	if lim := s.limitInt("limits.cloud_workers"); int64(running) >= lim {
		return &limitError{"limits.cloud_workers", lim}
	}
	workers, err := s.store.workers()
	if err != nil {
		return err
	}
	byNode := map[string]api.Worker{}
	for _, w := range workers {
		if w.Node != "" {
			byNode[w.Node] = w
		}
	}
	for _, node := range stopped {
		w, ok := byNode[node.ID]
		if !ok || w.Gone {
			continue
		}
		fmt.Fprintf(out, "Starting stopped worker %s...\n", w.Name)
		start := time.Now()
		if _, err := s.startWorker(ctx, f, w); err != nil {
			return err
		}
		fmt.Fprintf(out, "Worker %s started in %s.\n", w.Name, time.Since(start).Round(time.Second))
		return nil
	}

	name := "aws-" + randomHex(3)
	token, err := s.NewToken()
	if err != nil {
		return err
	}
	join, _ := json.Marshal(map[string]string{"server": f.cfg.Address, "token": token, "name": name})
	fmt.Fprintf(out, "Starting a new %s worker (%s)...\n", wtype, name)
	start := time.Now()
	node, err := f.p.ProvisionNode(ctx, provider.NodeSpec{Name: "hutch-" + name, Type: wtype, DiskGB: int(cfg.Size("worker_disk.size") >> 30),
		UserData: join, Tags: map[string]string{provider.TagWorker: name}})
	if err != nil {
		return fmt.Errorf("provision %s: %w", wtype, err)
	}
	_, secret, _ := strings.Cut(token, ".")
	s.store.setTokenNode(secret, node.ID)
	s.store.event("", "provisioned", fmt.Sprintf("%s %s (%s)", name, node.ID, wtype))
	if _, err := s.waitNode(ctx, node.ID, 10*time.Minute); err != nil {
		return fmt.Errorf("new worker %s (%s): %w", name, node.ID, err)
	}
	fmt.Fprintf(out, "Worker %s joined in %s.\n", name, time.Since(start).Round(time.Second))
	return nil
}

// startWorker starts a stopped elastic worker and waits for it to connect.
func (s *Server) startWorker(ctx context.Context, f *fleet, w api.Worker) (*workerConn, error) {
	if _, err := f.p.StartNode(ctx, w.Node); err != nil {
		return nil, fmt.Errorf("start %s: %w", w.Name, err)
	}
	s.store.setWorkerStopped(w.ID, time.Time{})
	s.store.event("", "started", w.Name)
	return s.waitNode(ctx, w.Node, 10*time.Minute)
}

// waitNode waits for the worker on a cloud node to connect.
func (s *Server) waitNode(ctx context.Context, node string, timeout time.Duration) (*workerConn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if ws, err := s.store.workers(); err == nil {
			for _, w := range ws {
				if w.Node == node {
					if c, err := s.worker(w.ID); err == nil {
						return c, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("didn't connect within %s", timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

// wakeStopped starts the stopped elastic worker a box is on, so the box wakes where
// its files are. It returns nil when the worker isn't a stopped elastic worker.
func (s *Server) wakeStopped(ctx context.Context, workerID string) (*workerConn, error) {
	f := s.fleet.Load()
	if f == nil {
		return nil, nil
	}
	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.ID == workerID && w.Node != "" && !w.Gone && !w.Stopped.IsZero() {
			f.grow.Lock()
			defer f.grow.Unlock()
			if c, err := s.worker(w.ID); err == nil {
				return c, nil
			}
			if lim := s.limitInt("limits.cloud_workers"); int64(s.cloudRunning(ctx, f)) >= lim {
				return nil, &limitError{"limits.cloud_workers", lim}
			}
			return s.startWorker(ctx, f, w)
		}
	}
	return nil, nil
}

func (s *Server) cloudRunning(ctx context.Context, f *fleet) int {
	nodes, err := f.listNodes(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, node := range nodes {
		if node.State == provider.Pending || node.State == provider.Running {
			n++
		}
	}
	return n
}

// teardownLoop stops elastic workers that have had no awake boxes (and nothing left
// to upload) for providers.teardown_after, and keeps the stopped pool in bounds.
// Nothing is stopped or destroyed while a worker can't be seen: a worker that isn't
// connected is left alone.
func (s *Server) teardownLoop(ctx context.Context) {
	empty := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		f := s.fleet.Load()
		if f == nil {
			continue
		}
		if err := s.teardown(ctx, f, empty); err != nil {
			log.Printf("teardown: %v", err)
		}
	}
}

func (s *Server) teardown(ctx context.Context, f *fleet, empty map[string]time.Time) error {
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	workers, err := s.store.workers()
	if err != nil {
		return err
	}
	boxes, err := s.store.boxes()
	if err != nil {
		return err
	}
	awake := map[string]int{}
	for _, b := range boxes {
		if b.State == "running" {
			awake[b.Worker]++
		}
	}
	terminate := cfg.String("providers.empty_worker") == "terminate"
	now := time.Now()
	for _, w := range workers {
		if w.Node == "" || w.Gone {
			continue
		}
		s.mu.Lock()
		c := s.workers[w.ID]
		busy := c == nil || awake[w.ID] > 0 || s.pending[w.ID] > 0 || c.uploading > 0
		// A worker started early waits providers.prestart_timeout for its first box.
		after := cfg.Duration("providers.teardown_after")
		if busy {
			delete(s.prestarted, w.ID)
		} else if s.prestarted[w.ID] {
			after = cfg.Duration("providers.prestart_timeout")
		}
		s.mu.Unlock()
		if busy {
			delete(empty, w.ID)
			continue
		}
		if _, ok := empty[w.ID]; !ok {
			empty[w.ID] = now
			continue
		}
		if now.Sub(empty[w.ID]) < after {
			continue
		}
		delete(empty, w.ID)
		if terminate {
			s.destroyWorker(ctx, f, w, fmt.Sprintf("empty for %s", after))
			continue
		}
		if err := f.p.StopNode(ctx, w.Node); err != nil {
			log.Printf("stop %s: %v", w.Name, err)
			continue
		}
		s.store.setWorkerStopped(w.ID, now)
		s.store.event("", "stopped", fmt.Sprintf("%s, empty for %s", w.Name, after))
		log.Printf("stopped worker %s (%s), empty for %s", w.Name, w.Node, after)
	}

	// The stopped pool: the newest providers.stopped_pool of the current type stay.
	nodes, err := f.listNodes(ctx)
	if err != nil {
		return err
	}
	// Again, with the stop times just recorded.
	if workers, err = s.store.workers(); err != nil {
		return err
	}
	state := map[string]provider.Node{}
	for _, n := range nodes {
		state[n.ID] = n
	}
	var pool []api.Worker
	for _, w := range workers {
		n, ok := state[w.Node]
		if w.Node == "" || w.Gone {
			continue
		}
		if !ok {
			// Destroyed outside Hutch: its boxes restore elsewhere.
			s.store.setWorkerGone(w.ID)
			s.store.event("", "gone", w.Name+": its instance no longer exists")
			continue
		}
		if n.State != provider.Stopped {
			continue
		}
		switch {
		case n.Type != cfg.String("providers.aws.worker_type"):
			s.destroyWorker(ctx, f, w, "an old worker type")
		case f.cfg.AWS != nil && n.Image != "" && n.Image != f.cfg.AWS.ImageID:
			s.destroyWorker(ctx, f, w, "an old worker image")
		case !w.Stopped.IsZero() && now.Sub(w.Stopped) > cfg.Duration("providers.stopped_ttl"):
			s.destroyWorker(ctx, f, w, "stopped longer than providers.stopped_ttl")
		default:
			pool = append(pool, w)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Stopped.After(pool[j].Stopped) })
	for _, w := range pool[min(len(pool), int(cfg.Int("providers.stopped_pool"))):] {
		s.destroyWorker(ctx, f, w, "beyond providers.stopped_pool")
	}
	return nil
}

// destroyWorker terminates an elastic worker. Its sleeping boxes are in the bucket
// and restore on another worker when used. A worker holding a box the bucket
// doesn't have (a template box, a box outside projects, a snapshot not uploaded
// yet) is kept.
func (s *Server) destroyWorker(ctx context.Context, f *fleet, w api.Worker, why string) {
	boxes, err := s.store.boxes()
	if err != nil {
		return
	}
	for _, b := range boxes {
		if b.Worker != w.ID {
			continue
		}
		store, err := s.storeOf(b)
		if err == nil {
			var gen int
			if gen, err = s.latestGeneration(ctx, store); err == nil && gen < b.Generation {
				err = fmt.Errorf("its snapshot %d isn't uploaded", b.Generation)
			}
		}
		if err != nil {
			log.Printf("keeping worker %s (%s): %s isn't in the bucket: %v", w.Name, why, b.ID, err)
			return
		}
	}
	if err := f.p.DestroyNode(ctx, w.Node); err != nil {
		log.Printf("destroy %s: %v", w.Name, err)
		return
	}
	s.store.setWorkerGone(w.ID)
	s.store.event("", "destroyed", fmt.Sprintf("%s: %s", w.Name, why))
	log.Printf("destroyed worker %s (%s): %s", w.Name, w.Node, why)
}

// providerAdd connects the server to AWS. The server must run on EC2: new workers
// go in its subnet and security group, and dial its private address.
func (s *Server) providerAdd(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProviderParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil, fmt.Errorf("the server listens on %s, which workers can't reach: run it with --listen 0.0.0.0:%s", s.cfg.Listen, port)
	}
	md, err := instanceMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("the server must run on EC2 to add AWS (instance metadata: %w)", err)
	}
	address := net.JoinHostPort(md.privateIP, port)
	found := false
	for _, n := range s.cfg.Names {
		found = found || n == md.privateIP
	}
	if !found {
		return nil, fmt.Errorf("start the server with --name %s, so new workers can verify it at %s", md.privateIP, address)
	}
	fleetID := p.Fleet
	if fleetID == "" {
		fleetID = pkiFleet(s)
	}
	cfg := aws.Config{Region: md.region, Credentials: p.Credentials, Fleet: fleetID, SubnetID: md.subnet,
		SecurityGroupID: md.securityGroup, InstanceProfile: p.InstanceProfile}
	a, err := aws.New(cfg)
	if err != nil {
		return nil, err
	}
	var rootGB int
	if cfg.ImageID, rootGB, err = a.WorkerImage(ctx); err != nil {
		return nil, err
	}
	if err := a.AllowWithinGroup(ctx, md.securityGroup, port); err != nil {
		return nil, fmt.Errorf("let workers reach port %s: %w", port, err)
	}
	if _, err := a.ListNodes(ctx); err != nil {
		return nil, err
	}
	if v := s.vault.Load(); v != nil && v.cfg.Endpoint == "" {
		if b, ok := v.bucket.(interface {
			Tag(context.Context, map[string]string) error
		}); ok {
			if err := b.Tag(ctx, map[string]string{provider.TagFleet: cfg.Fleet}); err != nil {
				log.Printf("tag bucket: %v", err)
			}
		}
	}
	fc := fleetConfig{AWS: &cfg, Address: address, RootGB: rootGB, ServerDiskGB: p.ServerDiskGB}
	f, err := newFleet(fc)
	if err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(fc, "", "  ")
	if err := os.WriteFile(filepath.Join(s.cfg.Dir, "provider.json"), data, 0o600); err != nil {
		return nil, err
	}
	s.fleet.Store(f)
	log.Printf("provider aws: region %s, image %s, workers dial %s", md.region, cfg.ImageID, address)
	return api.ProviderInfo{Region: md.region, Image: cfg.ImageID, Address: address}, nil
}

// pkiFleet names this server's fleet in tags: its CA fingerprint, so a restored
// server finds its own workers.
func pkiFleet(s *Server) string { return "f-" + fingerprint(s)[:16] }

type metadata struct {
	region, privateIP, subnet, securityGroup string
}

// instanceMetadata reads where the server's own instance is, through IMDSv2.
func instanceMetadata(ctx context.Context) (metadata, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := c.Do(req)
	if err != nil {
		return metadata{}, err
	}
	token, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	get := func(path string) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/"+path, nil)
		req.Header.Set("X-aws-ec2-metadata-token", string(token))
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: %s", path, resp.Status)
		}
		return strings.TrimSpace(string(b)), nil
	}
	var md metadata
	if md.region, err = get("placement/region"); err != nil {
		return md, err
	}
	if md.privateIP, err = get("local-ipv4"); err != nil {
		return md, err
	}
	mac, err := get("mac")
	if err != nil {
		return md, err
	}
	if md.subnet, err = get("network/interfaces/macs/" + mac + "/subnet-id"); err != nil {
		return md, err
	}
	groups, err := get("network/interfaces/macs/" + mac + "/security-group-ids")
	if err != nil {
		return md, err
	}
	md.securityGroup, _, _ = strings.Cut(groups, "\n")
	return md, nil
}

// prestart starts a worker in the background when providers.prestart is on, none
// has room for a box of the given size, and limits allow it. It returns at once.
func (s *Server) prestart(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.PrestartParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	f := s.fleet.Load()
	v, err := s.limits("providers.prestart")
	if err != nil || f == nil || v != true {
		return nil, err
	}
	n := need{memMiB: p.MemMiB, vcpus: p.VCPUs}
	if _, release, err := s.place(n, ""); err == nil {
		release()
		return nil, nil
	}
	go func() {
		ctx := context.Background()
		before := map[string]bool{}
		for _, w := range s.connectedWorkers() {
			before[w.id] = true
		}
		if err := s.grow(ctx, f, n, io.Discard); err != nil {
			log.Printf("prestart: %v", err)
			return
		}
		s.mu.Lock()
		for id := range s.workers {
			if !before[id] {
				s.prestarted[id] = true
				log.Printf("prestarted worker %s", id)
			}
		}
		s.mu.Unlock()
	}()
	return nil, nil
}
