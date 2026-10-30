package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/mcp"
	"github.com/mouizahmed/hutch/internal/rpc"
)

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("diagnostic output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func (s *Server) diagnosticOutput(ctx context.Context, w *workerConn, id string, argv []string, limit int) ([]byte, error) {
	var out = boundedBuffer{limit: limit}
	var errout = boundedBuffer{limit: 4096}
	code, err := rpc.Exec(ctx, w.sess, api.BoxExec, api.ExecParams{ID: id, Dir: workspace, Argv: argv}, &out, &errout)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		// The diagnostic helpers suppress provider bodies and subprocess
		// stderr; their own errors contain only the failed check and cause.
		return nil, fmt.Errorf("diagnostic command exited %d: %s", code, strings.TrimSpace(errout.String()))
	}
	return out.Bytes(), nil
}

func decodeFingerprint(data []byte) (map[string]string, error) {
	var result map[string]string
	if err := json.Unmarshal(data, &result); err != nil || len(result["working files"]) != 64 {
		return nil, fmt.Errorf("invalid build fingerprint")
	}
	return result, nil
}

func changedFingerprints(first, next map[string]string) []string {
	names := map[string]bool{}
	for name, digest := range first {
		if next[name] != digest {
			names[name] = true
		}
	}
	for name, digest := range next {
		if first[name] != digest {
			names[name] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

func (s *Server) doctorTemplate(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.TemplateParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if p.Commit == "" || p.BuildTimeout <= 0 {
		return nil, fmt.Errorf("doctor needs a committed recipe and a build timeout")
	}
	proj, err := s.store.project(p.Project)
	if err != nil {
		return nil, err
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	var checks []api.Check
	add := func(name, status, detail, fix string) {
		checks = append(checks, api.Check{Area: "template", Name: name, Status: status, Detail: detail, Fix: fix})
	}
	if p.Recipe["verify.sh"] == nil {
		add("verification", "warn", "no .hutch/verify.sh; project behavior is not checked", "add a deterministic .hutch/verify.sh")
	}
	add("scope", "warn", "Dockerfile steps run twice without layer reuse; fingerprints cover package/tool versions and non-ignored project files, not entire disks or native-client OAuth", "use .hutch/fingerprint.sh for extra deterministic checks; verify native-client authentication separately")
	var first map[string]string
	for n := 1; n <= 2; n++ {
		nonce := make([]byte, 8)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		id := "doctor-" + hex.EncodeToString(nonce)
		// Never replace an existing box even if an identifier collides.
		if _, err := s.store.box(id); err == nil {
			return nil, fmt.Errorf("diagnostic identifier collision")
		}
		fmt.Fprintf(stdout, "\nDiagnostic build %d of 2 (%.12s)\n", n, p.Commit)
		buildErr := func() error {
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := s.store.box(id); err == nil {
					if err := s.removeBox(cleanup, id); err != nil {
						add("cleanup "+id, "fail", "diagnostic box could not be removed", "hutch rm "+id)
						return
					}
				}
				// A failed image build may not have a box row yet. Use the
				// random diagnostic name on each worker; removal is idempotent
				// and the worker refuses names outside this namespace.
				workers, err := s.store.workers()
				if err != nil {
					add("image cleanup "+id, "warn", "could not enumerate workers", "check the machine's diagnostic image directory")
					return
				}
				for _, worker := range workers {
					if worker.Gone {
						continue
					}
					w, err := s.worker(worker.ID)
					if err == nil {
						err = rpc.Call(cleanup, w.sess, api.ImageRemoveDiagnostic, api.IDParams{ID: id}, nil)
					}
					if err != nil {
						add("image cleanup "+id, "warn", "diagnostic image cleanup could not reach a worker", "check the machine's diagnostic image directory")
					}
				}
			}()
			if _, err := s.buildBox(ctx, proj, p, id, stdout, stderr, true); err != nil {
				return err
			}
			w, err := s.workerFor(id)
			if err != nil {
				return err
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			manifest, err := s.diagnosticOutput(cctx, w, id, []string{"/usr/local/bin/hutch-guest", "fingerprint"}, 64<<10)
			if err != nil {
				return fmt.Errorf("fingerprint: %w", err)
			}
			fingerprint, err := decodeFingerprint(manifest)
			if err != nil {
				return err
			}
			meta, err := s.store.meta(id)
			if err != nil {
				return err
			}
			sources := strings.Split(meta.image, ", ")
			slices.Sort(sources)
			fingerprint["image sources"] = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(sources, "\n"))))
			if n == 1 {
				first = fingerprint
			} else if changes := changedFingerprints(first, fingerprint); len(changes) > 0 {
				add("reproducibility", "fail", "fresh builds differ: "+strings.Join(changes, ", "), "pin dependencies and installers; use .hutch/fingerprint.sh for additional deterministic checks")
			} else {
				add("reproducibility", "ok", "two builds passed; package/tool versions and working-file fingerprints match", "")
			}
			// MCP commands run in the diagnostic box as dev. Only initialization
			// and inventory are requested; no model turn or tool is executed.
			for _, a := range set.Sorted() {
				if a.MCP == nil {
					continue
				}
				probe, cancel := context.WithTimeout(ctx, 130*time.Second)
				data, err := s.diagnosticOutput(probe, w, id, []string{"/usr/local/bin/hutch-guest", "mcp-check", "--file", a.MCP.File, "--key", a.MCP.Key}, 1<<20)
				cancel()
				if err != nil {
					add(fmt.Sprintf("build %d %s MCP", n, a.Name), "fail", "MCP config could not be checked", "check its config and commands in a box")
					continue
				}
				var results []mcp.Result
				if err := json.Unmarshal(data, &results); err != nil {
					return fmt.Errorf("invalid MCP diagnostic result")
				}
				for _, r := range results {
					fix := ""
					if r.Status != "ok" {
						fix = "check this server with the agent's native MCP client"
					}
					add(fmt.Sprintf("build %d %s MCP %s", n, a.Name, r.Name), r.Status, r.Detail, fix)
				}
			}
			return nil
		}()
		if buildErr != nil {
			add(fmt.Sprintf("build %d", n), "fail", buildErr.Error(), "fix the recipe, verification, or missing tools, then rerun hutch doctor --template")
			break
		}
	}
	// Surface no secrets, fingerprint content, or provider error payloads.
	for i := range checks {
		checks[i].Detail = strings.TrimSpace(checks[i].Detail)
	}
	return api.DoctorResult{Checks: checks}, nil
}
