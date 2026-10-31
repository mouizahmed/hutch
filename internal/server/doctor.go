package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
)

// hello refuses a CLI too old for this hutchd and says what it runs.
func (s *Server) hello(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.Handshake
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if p.Protocol < api.Protocol-1 {
		return nil, fmt.Errorf("hutch %s on this computer is too old for the machine's hutchd %s: upgrade hutch on this computer", p.Version, version.String())
	}
	return api.Handshake{Protocol: api.Protocol, Version: version.String()}, nil
}

// doctor checks the server, its bucket, every worker, and the boxes,
// each failure with a fix.
func (s *Server) doctor(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.DoctorParams
	if len(params) > 0 {
		if err := rpc.Decode(params, &p); err != nil {
			return nil, err
		}
	}
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
	check("config", "machine and user layers", err, "", "hutch config validate, then hutch config edit --machine or hutch config edit")
	if err != nil {
		return r, nil
	}
	check("config", "egress", cfg.Unsupported(), cfg.String("network.egress"), `hutch config set network.egress open (in the layer that sets it)`)
	r.Checks = append(r.Checks, s.doctorUser(cmp.Or(p.DefaultAgent, cfg.String("default_agent")))...)

	if v := s.vault.Load(); v == nil {
		warn("machine", "bucket", "none: losing the machine loses its boxes", "hutch machine bucket")
	} else {
		check("machine", "bucket", checkBucket(ctx, v.bucket), v.cfg.Name, "check the bucket's credentials, and that it supports conditional writes")
		s.backupMu.Lock()
		last, at := s.backedUp, s.lastBackup
		s.backupMu.Unlock()
		if n, err := s.store.changes(); err != nil {
			check("machine", "backup", err, "", "see journalctl -u hutchd on the machine")
		} else if detail := backupWarning(n, last, at, s.started, time.Now(), cfg.Duration("backup.warn_after")); detail != "" {
			warn("machine", "backup", detail, "see journalctl -u hutchd on the machine for backup errors")
		} else {
			check("machine", "backup", nil, "current", "")
		}
	}

	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	reserve := cfg.Percent("machine.disk_reserve")
	for _, w := range ws {
		if w.Gone {
			continue
		}
		area := "machine"
		conn, err := s.worker(w.ID)
		switch {
		case err != nil:
			warn(area, "state", fmt.Sprintf("offline since %s", w.LastSeen.Format(time.DateTime)),
				"start hutchd there: sudo systemctl start hutchd")
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
			warn(area, "disk", fmt.Sprintf("%d%% free, below machine.disk_reserve; it takes no new boxes", 100*d.Free/d.Total), "remove boxes there, or give it a larger disk")
		}
		if conn.info.Version != r.Version {
			warn(area, "version", "worker "+conn.info.Version+", hutchd "+r.Version, "restart hutchd there")
		}
	}

	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].ID < boxes[j].ID })
	// A box from before the keys proxy holds real logins and refreshes them
	// itself, which retires the refresh token every other box uses.
	var legacy []string
	for _, b := range boxes {
		creds, _ := s.store.boxCredentials(b.ID)
		if b.State != "running" || len(creds) == 0 {
			continue
		}
		if w, err := s.worker(b.Worker); err == nil {
			var v int
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if rpc.Call(cctx, w.sess, api.BoxGuestVersion, api.IDParams{ID: b.ID}, &v) == nil && v < 3 {
				legacy = append(legacy, b.ID)
			}
			cancel()
		}
	}
	if len(legacy) > 0 {
		warn("logins", "real logins in boxes", strings.Join(legacy, ", ")+" predate the keys proxy: their agents refresh logins themselves, which logs every other box out",
			"move their work to new boxes and remove them, then hutch login again")
	}
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
		check("boxes", "fit", nil, fmt.Sprintf("all %d fit the machine", len(boxes)), "")
	}
	return r, nil
}

func backupWarning(changes, backedUp int64, at, started, now time.Time, after time.Duration) string {
	if changes == backedUp {
		return ""
	}
	if at.IsZero() {
		if now.Sub(started) <= after {
			return ""
		}
		return fmt.Sprintf("no successful backup since hutchd started, %s ago", now.Sub(started).Round(time.Second))
	}
	if now.Sub(at) > after {
		return fmt.Sprintf("changes since the last backup, %s ago", now.Sub(at).Round(time.Second))
	}
	return ""
}
