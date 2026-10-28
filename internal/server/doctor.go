package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

// doctor checks the server, its bucket, every worker, and the boxes,
// each failure with a fix.
func (s *Server) doctor(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	r := api.DoctorResult{Version: version.String()}
	check := func(area, name string, err error, detail, fix string) {
		c := api.Check{Area: area, Name: name, Status: "ok", Detail: detail}
		if err != nil {
			c.Status, c.Detail, c.Fix = "fail", err.Error(), fix
		}
		r.Checks = append(r.Checks, c)
	}
	warn := func(area, name, detail, fix string) {
		r.Checks = append(r.Checks, api.Check{Area: area, Name: name, Status: "warn", Detail: detail, Fix: fix})
	}

	cfg, err := s.settings()
	check("config", "server and user layers", err, "", "hutch config validate, then hutch config edit --server or hutch config edit")
	if err != nil {
		return r, nil
	}

	if v := s.vault.Load(); v == nil {
		check("server", "bucket", fmt.Errorf("no bucket: nothing is backed up"), "", "hutch server init")
	} else {
		check("server", "bucket", checkBucket(ctx, v.bucket), v.cfg.Name, "check the bucket's credentials, and that it supports conditional writes")
		s.backupMu.Lock()
		last, at := s.backedUp, s.lastBackup
		s.backupMu.Unlock()
		if n, err := s.store.changes(); err == nil && n != last && !at.IsZero() && time.Since(at) > cfg.Duration("backup.warn_after") {
			warn("server", "backup", fmt.Sprintf("changes since the last backup, %s ago", time.Since(at).Round(time.Second)), "see hutch logs server for backup errors")
		} else {
			check("server", "backup", nil, "current", "")
		}
	}

	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	reserve := cfg.Percent("worker.disk_reserve")
	for _, w := range ws {
		if w.Gone {
			continue
		}
		area := "worker " + w.Name
		conn, err := s.worker(w.ID)
		switch {
		case err != nil:
			warn(area, "state", fmt.Sprintf("offline since %s", w.LastSeen.Format(time.DateTime)),
				"start hutchd there, or hutch workers rm "+w.ID+" if it's gone (its stored boxes restore elsewhere)")
			continue
		}
		var checks []api.Check
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = rpc.Call(cctx, conn.sess, api.WorkerDoctor, struct{}{}, &checks)
		cancel()
		if err != nil {
			warn(area, "checks", "this hutchd can't run them: "+err.Error(), "upgrade hutchd on it")
		}
		for _, c := range checks {
			c.Area = area
			r.Checks = append(r.Checks, c)
		}
		if d := conn.info.Disk; d.Total > 0 && 100*float64(d.Free)/float64(d.Total) < reserve {
			warn(area, "disk", fmt.Sprintf("%d%% free, below worker.disk_reserve; it takes no new boxes", 100*d.Free/d.Total), "remove boxes there, or give it a larger disk")
		}
		if conn.info.Version != r.Version {
			warn(area, "version", "hutchd "+conn.info.Version+", server "+r.Version, "upgrade hutchd there")
		}
	}

	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].ID < boxes[j].ID })
	stranded := 0
	for _, b := range boxes {
		if b.Kind != "" {
			continue
		}
		if err := s.checkFits(ctx, need{memMiB: b.MemMiB, vcpus: b.VCPUs}); err != nil {
			stranded++
			check("boxes", b.ID, err, "", "hutch resize "+b.ID+" --cpu N --memory SIZE, or a bigger machine")
		}
	}
	if stranded == 0 {
		check("boxes", "fit", nil, fmt.Sprintf("all %d fit a worker", len(boxes)), "")
	}
	return r, nil
}
