package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/provider"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Cost categories, as hutch costs shows them.
const (
	costCompute  = "compute"
	costStorage  = "storage"
	costTransfer = "transfer"
	costOther    = "other"
)

// prices are the provider's, refreshed daily.
type prices struct {
	perHour map[string]float64 // instance type → $/hr
	rates   provider.Rates
	at      time.Time
}

// listNodes lists the fleet's workers, keeping the list for perHour and the
// server instance (when Hutch created it) for costs.
func (f *fleet) listNodes(ctx context.Context) ([]provider.Node, error) {
	all, err := f.p.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var workers []provider.Node
	var server *provider.Node
	for _, n := range all {
		if n.Worker() {
			workers = append(workers, n)
		} else {
			server = &n
		}
	}
	f.nodesMu.Lock()
	f.nodes, f.server = workers, server
	f.nodesMu.Unlock()
	return workers, nil
}

func (f *fleet) prices(ctx context.Context) (map[string]float64, provider.Rates, error) {
	f.pricesMu.Lock()
	defer f.pricesMu.Unlock()
	if f.price.perHour != nil && time.Since(f.price.at) < 24*time.Hour {
		return f.price.perHour, f.price.rates, nil
	}
	types, err := f.instanceTypes(ctx)
	if err != nil {
		return nil, provider.Rates{}, err
	}
	rates, err := f.p.Rates(ctx)
	if err != nil {
		return nil, provider.Rates{}, err
	}
	perHour := map[string]float64{}
	for _, t := range types {
		perHour[t.Name] = t.PerHour
	}
	f.price = prices{perHour: perHour, rates: rates, at: time.Now()}
	return perHour, rates, nil
}

// costLoop adds what the fleet cost each minute to the ledger, and once a day
// compares yesterday's estimate with the provider's bill.
func (s *Server) costLoop(ctx context.Context) {
	// Costs ran on while the server was down (disks of stopped workers, the
	// bucket): the first pass covers that gap, without the server's own compute.
	last := time.Now()
	if t, err := time.Parse(time.RFC3339, s.store.state("costs.accrued")); err == nil && t.Before(last) {
		last = t
	}
	serverUp := false
	reconciled := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		now := time.Now()
		f := s.fleet.Load()
		if f == nil {
			last = now
			continue
		}
		if err := s.accrue(ctx, f, now.Sub(last), serverUp); err != nil {
			log.Printf("costs: %v", err)
		} else {
			last, serverUp = now, true
			s.store.setState("costs.accrued", now.UTC().Format(time.RFC3339))
		}
		// Cost Explorer lags about a day; check yesterday once it's well past midnight.
		yesterday := now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
		if now.UTC().Hour() >= 12 && reconciled != yesterday {
			if err := s.reconcile(ctx, f, yesterday); err != nil {
				log.Printf("costs: reconcile %s: %v", yesterday, err)
			} else {
				reconciled = yesterday
			}
		}
	}
}

// accrue adds dt of the fleet's running costs to today's ledger. A worker's price
// is split across the boxes awake on it by their share of its memory; the rest, and
// disks, addresses, and the bucket, are fleet overhead (box "").
func (s *Server) accrue(ctx context.Context, f *fleet, dt time.Duration, serverUp bool) error {
	perHour, rates, err := f.prices(ctx)
	if err != nil {
		return err
	}
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	nodes, err := f.listNodes(ctx)
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
	byNode := map[string]api.Worker{}
	for _, w := range workers {
		byNode[w.Node] = w
	}
	hours := dt.Hours()
	months := hours / 730
	add := map[[2]string]float64{}
	diskGB := float64(cfg.Size("worker_disk.size")>>30) + float64(f.cfg.RootGB)
	for _, n := range nodes {
		add[[2]string{"", costStorage}] += diskGB * rates.DiskGBMonth * months
		if n.State != provider.Running && n.State != provider.Pending {
			continue
		}
		add[[2]string{"", costOther}] += rates.IPv4Hour * hours
		compute := perHour[n.Type] * hours
		w, ok := byNode[n.ID]
		if !ok || w.MemMiB == 0 {
			add[[2]string{"", costCompute}] += compute
			continue
		}
		left := compute
		for _, b := range boxes {
			if b.Worker == w.ID && b.State == "running" {
				share := compute * float64(b.MemMiB) / float64(w.MemMiB)
				add[[2]string{b.ID, costCompute}] += share
				left -= share
			}
		}
		add[[2]string{"", costCompute}] += math.Max(left, 0)
	}
	f.nodesMu.Lock()
	server := f.server
	f.nodesMu.Unlock()
	if server != nil {
		if server.State == provider.Running && serverUp {
			add[[2]string{"", costCompute}] += perHour[server.Type] * hours
			add[[2]string{"", costOther}] += rates.IPv4Hour * hours
		}
		add[[2]string{"", costStorage}] += float64(f.cfg.ServerDiskGB) * rates.DiskGBMonth * months
	}
	add[[2]string{"", costStorage}] += float64(s.bucketBytes.Load()) / (1 << 30) * rates.BucketGBMonth * months
	t1, t2 := s.takeRequests()
	add[[2]string{"", costStorage}] += float64(t1)/1000*rates.PutPer1000 + float64(t2)/1000*rates.GetPer1000
	return s.store.addCosts(time.Now().UTC().Format(time.DateOnly), add)
}

// countRequests adds a worker's cumulative bucket requests since its last report.
func (s *Server) countRequests(w *workerConn, total [2]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range total {
		d := total[i] - w.requests[i]
		if d < 0 { // hutchd restarted or reconnected to a new bucket client
			d = total[i]
		}
		s.requests[i] += d
		w.requests[i] = total[i]
	}
}

// takeRequests returns the bucket requests made since the last call, by the
// server's own client and every worker's.
func (s *Server) takeRequests() (int64, int64) {
	var own [2]int64
	if v := s.vault.Load(); v != nil {
		if b, ok := v.bucket.(interface{ Requests() (int64, int64) }); ok {
			own[0], own[1] = b.Requests()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range own {
		d := own[i] - s.ownRequests[i]
		if d < 0 {
			d = own[i]
		}
		s.requests[i] += d
		s.ownRequests[i] = own[i]
	}
	t1, t2 := s.requests[0], s.requests[1]
	s.requests = [2]int64{}
	return t1, t2
}

// bucketSizeLoop measures the bucket hourly, for storage costs.
func (s *Server) bucketSizeLoop(ctx context.Context) {
	for {
		if v := s.vault.Load(); v != nil {
			if objs, err := v.bucket.List(ctx, ""); err == nil {
				var total int64
				for _, o := range objs {
					total += o.Size
				}
				s.bucketBytes.Store(total)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// reconcile stores the provider's bill for a day next to the estimate, and
// notifies when they differ by more than costs.reconcile_tolerance.
func (s *Server) reconcile(ctx context.Context, f *fleet, day string) error {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return err
	}
	lines, err := f.p.BilledCosts(ctx, t)
	if err != nil {
		return err
	}
	billed := map[string]float64{}
	var total float64
	for _, l := range lines {
		billed[l.Category] += l.USD
		total += l.USD
	}
	if err := s.store.setBilled(day, billed); err != nil {
		return err
	}
	est, err := s.store.costTotal(day)
	if err != nil {
		return err
	}
	cfg, err := s.settings()
	if err != nil {
		return err
	}
	tol := cfg.Percent("costs.reconcile_tolerance") / 100
	if est > 0.01 && math.Abs(total-est)/est > tol {
		msg := fmt.Sprintf("%s: AWS billed $%.2f, Hutch estimated $%.2f", day, total, est)
		s.store.event("", "costs", msg)
		s.notify("costs", "", "Costs differ from the bill", msg)
	}
	return nil
}

// spendReached reports whether today's estimate has reached limits.cloud_spend_per_day.
func (s *Server) spendReached() (*limitError, error) {
	v, err := s.limits("limits.cloud_spend_per_day")
	if err != nil {
		return nil, err
	}
	max := v.(int64)
	spent, err := s.store.costTotal(time.Now().UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	if max > 0 && spent >= float64(max) {
		return &limitError{"limits.cloud_spend_per_day", max}, nil
	}
	return nil, nil
}

func (s *Server) costsReport(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.CostsParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	from := time.Now().UTC().AddDate(0, 0, -max(p.Days, 1)+1).Format(time.DateOnly)
	rows, err := s.store.costs(from)
	if err != nil {
		return nil, err
	}
	if p.By == "project" {
		boxes, err := s.store.boxes()
		if err != nil {
			return nil, err
		}
		project := map[string]string{}
		for _, b := range boxes {
			project[b.ID] = b.Project
		}
		for i := range rows {
			if rows[i].Box != "" {
				rows[i].Box = project[rows[i].Box]
			}
		}
	}
	billed, err := s.store.billed(from)
	if err != nil {
		return nil, err
	}
	return api.Costs{Rows: rows, Billed: billed}, nil
}

// perHour returns what each awake box costs now: its share of its worker's price.
func (s *Server) perHour(ctx context.Context, boxes []api.Box) map[string]float64 {
	f := s.fleet.Load()
	if f == nil {
		return nil
	}
	perHour, _, err := f.prices(ctx)
	if err != nil {
		return nil
	}
	workers, err := s.store.workers()
	if err != nil {
		return nil
	}
	f.nodesMu.Lock()
	nodeType := map[string]string{}
	for _, n := range f.nodes {
		nodeType[n.ID] = n.Type
	}
	f.nodesMu.Unlock()
	byID := map[string]api.Worker{}
	for _, w := range workers {
		byID[w.ID] = w
	}
	out := map[string]float64{}
	for _, b := range boxes {
		w := byID[b.Worker]
		if b.State != "running" || w.Node == "" || w.MemMiB == 0 {
			continue
		}
		out[b.ID] = perHour[nodeType[w.Node]] * float64(b.MemMiB) / float64(w.MemMiB)
	}
	return out
}

// workerPerHour returns each running elastic worker's price.
func (s *Server) workerPerHour(ctx context.Context) map[string]float64 {
	f := s.fleet.Load()
	if f == nil {
		return nil
	}
	perHour, _, err := f.prices(ctx)
	if err != nil {
		return nil
	}
	f.nodesMu.Lock()
	defer f.nodesMu.Unlock()
	out := map[string]float64{}
	for _, n := range f.nodes {
		if n.State == provider.Running || n.State == provider.Pending {
			out[n.ID] = perHour[n.Type]
		}
	}
	return out
}
