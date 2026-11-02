//go:build linux

// Package agent serves the server's calls to the boxes on this machine.
package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/image"
	"github.com/mouizahmed/hutch/internal/keys"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/version"
	"github.com/mouizahmed/hutch/internal/wire"
	"github.com/mouizahmed/hutch/internal/worker"
)

// Agent serves the lead's calls to the boxes on this machine.
type Agent struct {
	w       *worker.Worker
	m       *monitor
	mux     rpc.Mux
	streams rpc.Streams
	server  atomic.Pointer[yamux.Session]
	// fenced says why this machine was fenced: its boxes stay asleep and its CLI
	// refuses until it joins a lead again.
	fenced atomic.Pointer[string]
	dir    link.Dir
	// Restart restarts hutchd: after taking the lead, it starts leading.
	Restart func()
}

// Start recovers this machine's boxes and starts watching them. dir is the
// worker's enrollment when another machine leads, else empty.
func Start(ctx context.Context, w *worker.Worker, name string, dir link.Dir) *Agent {
	// A host out-of-memory kill of hutchd would leave boxes unmanaged; kill VMs first.
	os.WriteFile("/proc/self/oom_score_adj", []byte("-900"), 0)
	kp := keys.NewProxy()
	w.Keys = kp
	w.Recover(ctx)
	u := newUploader(w)
	m := newMonitor(w, u)
	a := &Agent{w: w, m: m, dir: dir}
	kp.Lead = leadKeys{a}
	w.OpenKeys()
	go m.run(ctx, &a.server)
	go m.watchPressure(ctx)
	go u.run(ctx)
	a.mux, a.streams = handlers(w, name, m, u, kp, &image.Builder{Root: w.Root}, dir), streamHandlers(w, m)
	if dir != "" {
		a.mux[api.WorkerTakeLead] = a.takeLead
		a.mux[api.WorkerLead] = func(_ context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.LeadParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			log.Printf("link: the lead moved to %s (term %d)", p.Address, p.Term)
			if err := link.CheckTerm(dir, p.Term); err != nil {
				return nil, err
			}
			return nil, dir.SetLead(p.Address)
		}
	}
	return a
}

// Serve serves the lead's calls over conn until it closes. Boxes keep running.
func (a *Agent) Serve(ctx context.Context, conn net.Conn) error {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = log.New(log.Writer(), "yamux: ", log.LstdFlags)
	sess, err := yamux.Server(conn, cfg)
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	a.server.Store(sess)
	defer a.server.CompareAndSwap(sess, nil)
	if err := rpc.Serve(ctx, sess, a.mux, a.streams); err != nil {
		return err
	}
	return errors.New("connection closed")
}

// Link keeps a connection to another machine's lead, reconnecting with backoff
// up to machines.reconnect_max.
func (a *Agent) Link(ctx context.Context, dir link.Dir) {
	backoff := time.Second
	var followed time.Time
	for ctx.Err() == nil {
		start := time.Now()
		conn, err := link.Dial(ctx, dir)
		if err == nil {
			log.Printf("link: connected to the lead at %s", conn.RemoteAddr())
			err = a.Serve(ctx, conn)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("link: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		limit, every := 30*time.Second, 30*time.Second
		if p := a.m.settings.Load(); p != nil && p.ReconnectMax > 0 {
			limit, every = p.ReconnectMax, cmp.Or(p.LeadCheck, every)
		}
		// Without its lead, a worker follows the lead record to a newer one.
		if time.Since(followed) >= every {
			followed = time.Now()
			if moved, err := dir.Follow(ctx); err != nil {
				log.Printf("link: reading the lead record: %v", err)
			} else if moved {
				log.Printf("link: the lead record names a newer lead at %s; dialing it", dir.Lead())
				backoff = time.Second
				continue
			}
		}
		backoff = min(backoff*2, limit)
	}
}

func toAPI(w *worker.Worker, b *worker.Box) api.Box {
	state := string(b.State)
	if b.State == worker.Running && !w.Alive(b) {
		state = "crashed"
	}
	ab := api.Box{ID: b.ID, State: state, Image: b.Image, VCPUs: b.VCPUs, MemMiB: b.MemMiB,
		Generation: b.Generation, Uploaded: b.Uploaded, Class: b.Class, ReachIP: b.ReachIP(), Created: b.Created}
	if b.Store != nil {
		ab.Owner, ab.Project, ab.UID = b.Store.Owner, b.Store.Project, path.Base(b.Store.Prefix)
	}
	return ab
}

func toStore(s *api.Store) *worker.Store {
	if s == nil {
		return nil
	}
	return &worker.Store{Project: s.Project, Prefix: s.Prefix, Owner: s.Owner}
}

func handlers(w *worker.Worker, name string, m *monitor, u *uploader, kp *keys.Proxy, images *image.Builder, dir link.Dir) rpc.Mux {
	box := func(f func(context.Context, json.RawMessage) (*worker.Box, error)) rpc.Handler {
		return func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			b, err := f(ctx, params)
			if err != nil {
				return nil, err
			}
			// Report now, so a started box shows its threads' states without
			// waiting for the next check.
			if b.State == worker.Running {
				m.kick()
			}
			if b.Pending() {
				u.kick()
			}
			return toAPI(w, b), nil
		}
	}
	byID := func(op func(context.Context, string) (*worker.Box, error)) rpc.Handler {
		return box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return op(ctx, p.ID)
		})
	}
	return rpc.Mux{
		api.ImageBuild: func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ImageBuildParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			info, err := images.Get(p.Name)
			if err != nil || !p.Reuse || p.NoCache || info.Layer != images.LayerID() {
				stdout, _ := out.Writers()
				images.CacheBytes = p.CacheBytes
				switch {
				case p.Ref != "":
					info, err = images.FromRef(ctx, p.Name, p.Ref, p.Pins, stdout, p.NoCache)
				case p.Files != nil:
					info, err = images.Build(ctx, p.Name, p.Files, p.Pins, stdout, p.NoCache)
				case p.NoCache:
					info, err = images.BuildDefaultDiagnostic(ctx, p.Name, stdout)
				default:
					info, err = images.BuildDefault(ctx, stdout)
				}
				if err != nil {
					return nil, err
				}
			}
			res := api.Image{Name: info.Name, Built: info.Built}
			for _, s := range info.Sources {
				res.Sources = append(res.Sources, s.String())
			}
			return res, nil
		},
		api.ImageRemoveDiagnostic: func(_ context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return nil, images.RemoveDiagnostic(p.ID)
		},
		api.WorkerHello: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.LinkHello
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			if dir != "" {
				if err := link.CheckTerm(dir, p.Term); err != nil {
					return nil, err
				}
			}
			boxes, err := w.List()
			if err != nil {
				return nil, err
			}
			info := api.Worker{Name: name, Class: w.Class(), Classes: w.Classes(), Version: version.String(), CPUs: runtime.NumCPU(), MemMiB: hostMemMiB(),
				Arch: runtime.GOARCH, Disk: diskUsage(w.Root), Link: api.LinkProtocol}
			for _, b := range boxes {
				info.Boxes = append(info.Boxes, toAPI(w, b))
			}
			return info, nil
		},
		api.BoxBoot: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.BootParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Boot(ctx, p.ID, p.Image, p.VCPUs, p.MemMiB, p.DiskBytes, worker.Idle(p.Idle), p.Allow, toStore(p.Store))
		}),
		api.BoxFork: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.ForkParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Fork(ctx, p.Source, p.ID, worker.Idle(p.Idle), p.Allow, toStore(p.Store))
		}),
		api.WorkerSettings: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.WorkerSettingsParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			m.settings.Store(&p)
			if dir != "" && p.Bucket != nil {
				if err := dir.SaveBucket(*p.Bucket); err != nil {
					log.Printf("link: keeping the bucket's settings: %v", err)
				}
			}
			u.configure(&p)
			m.kick()
			return nil, nil
		},
		api.WorkerKeys: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var t keys.Table
			if err := rpc.Decode(params, &t); err != nil {
				return nil, err
			}
			return nil, kp.Set(t)
		},
		api.WorkerDoctor: func(context.Context, json.RawMessage, *rpc.Out) (any, error) {
			return append(doctor(w), placeChecks(w, m.settings.Load())...), nil
		},
		api.BoxRename: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.RenameParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Rename(p.ID, p.NewID, toStore(p.Store))
		}),
		api.BoxRestore: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.RestoreParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			s, k, ok := u.key(p.Store.Project)
			if !ok {
				return nil, fmt.Errorf("no key for project %s on this worker", p.Store.Project)
			}
			return w.Restore(ctx, p.ID, toStore(&p.Store), p.Generation, s, k)
		}),
		api.BoxArchive: byID(func(_ context.Context, id string) (*worker.Box, error) { return w.Archive(id) }),
		api.BoxResize: box(func(ctx context.Context, params json.RawMessage) (*worker.Box, error) {
			var p api.ResizeParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.Resize(p.ID, p.VCPUs, p.MemMiB, p.DiskBytes)
		}),
		api.BoxFiles: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.FilesParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return nil, w.WriteFiles(ctx, p.ID, p.Files)
		},
		api.BoxGuestVersion: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.GuestVersion(ctx, p.ID)
		},
		api.BoxStatus: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return w.GuestStatus(ctx, p.ID)
		},
		api.BoxSleep: byID(w.Sleep),
		api.BoxWake:  byID(w.Wake),
		api.BoxRm: func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
			var p api.IDParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return nil, w.Remove(p.ID)
		},
		api.BoxConsole: func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ConsoleParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			return rpc.ExitCode(0), followFile(ctx, w.ConsolePath(p.ID), 0, p.Follow, nil, out.Stdout)
		},
		api.BoxExec: func(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
			var p api.ExecParams
			if err := rpc.Decode(params, &p); err != nil {
				return nil, err
			}
			stdout, stderr := out.Writers()
			code, err := w.Exec(ctx, p.ID, guestproto.Exec{Argv: p.Argv, User: p.User, Dir: p.Dir, Env: p.Env}, stdout, stderr)
			if err != nil {
				return nil, err
			}
			return rpc.ExitCode(code), nil
		},
	}
}

func diskUsage(root string) api.Disk {
	var st syscall.Statfs_t
	if syscall.Statfs(root, &st) != nil {
		return api.Disk{}
	}
	return api.Disk{Total: int64(st.Blocks) * st.Bsize, Free: int64(st.Bavail) * st.Bsize}
}

func hostMemMiB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var kb int
	fmt.Sscanf(strings.TrimPrefix(string(data), "MemTotal:"), "%d", &kb)
	return kb / 1024
}

func streamHandlers(w *worker.Worker, m *monitor) rpc.Streams {
	return rpc.Streams{
		// The terminal stream passes straight through to hutch-guest, which speaks
		// the PTY frames with the CLI end to end.
		api.BoxPTY: func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			var p api.PTYParams
			if err := rpc.Decode(params, &p); err != nil {
				return err
			}
			g, err := w.DialGuest(ctx, p.ID)
			if err != nil {
				return err
			}
			req := guestproto.Request{Type: guestproto.TypePTY, Version: guestproto.Version,
				PTY: &guestproto.PTY{Argv: p.Argv, User: p.User, Term: p.Term, Rows: p.Rows, Cols: p.Cols}}
			if err := wire.Write(g, req); err != nil {
				g.Close()
				return err
			}
			rpc.Splice(conn, g)
			return nil
		},
		// Exec with input: guest frames end to end, like a terminal.
		api.BoxExecIn: func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			var p api.ExecParams
			if err := rpc.Decode(params, &p); err != nil {
				return err
			}
			if v, err := w.GuestVersion(ctx, p.ID); err != nil {
				return err
			} else if v < 2 {
				return fmt.Errorf("%s's hutch-guest predates input to exec; a box from a newer template has it", p.ID)
			}
			g, err := w.DialGuest(ctx, p.ID)
			if err != nil {
				return err
			}
			req := guestproto.Request{Type: guestproto.TypeExec, Version: guestproto.Version,
				Exec: &guestproto.Exec{Argv: p.Argv, User: p.User, Dir: p.Dir, Env: p.Env, Stdin: true}}
			if err := wire.Write(g, req); err != nil {
				g.Close()
				return err
			}
			rpc.Splice(conn, g)
			return nil
		},
		api.BoxDial: func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			var p api.DialParams
			if err := rpc.Decode(params, &p); err != nil {
				return err
			}
			g, err := w.DialPort(ctx, p.ID, p.Port)
			if err != nil {
				return err
			}
			if err := wire.Write(conn, guestproto.Response{Type: guestproto.RespOK}); err != nil {
				g.Close()
				return err
			}
			m.dial(p.ID, 1)
			defer m.dial(p.ID, -1)
			rpc.Splice(conn, g)
			return nil
		},
		// The serial console speaks the same frames as a terminal: Input in, Response out.
		api.BoxSerial: func(ctx context.Context, params json.RawMessage, conn net.Conn) error {
			var p api.ConsoleParams
			if err := rpc.Decode(params, &p); err != nil {
				return err
			}
			in, err := w.ConsoleInput(p.ID)
			if err != nil {
				return err
			}
			defer in.Close()
			info, err := os.Stat(w.ConsolePath(p.ID))
			if err != nil {
				return err
			}
			if err := wire.Write(conn, guestproto.Response{Type: guestproto.RespOK}); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() {
				defer cancel()
				for {
					var i guestproto.Input
					if wire.Read(conn, &i) != nil {
						return
					}
					if i.Type != guestproto.InputStdin {
						continue
					}
					if _, err := in.Write(i.Data); err != nil {
						return
					}
				}
			}()
			running := func() error {
				if b, err := w.Get(p.ID); err != nil || !w.Alive(b) {
					return fmt.Errorf("box %s stopped", p.ID)
				}
				return nil
			}
			return followFile(ctx, w.ConsolePath(p.ID), info.Size(), true, running, func(b []byte) error {
				return wire.Write(conn, guestproto.Response{Type: guestproto.RespStdout, Data: b})
			})
		},
	}
}

// followFile sends a file's contents from offset, then with follow keeps sending what
// is appended until ctx ends or check fails.
func followFile(ctx context.Context, path string, offset int64, follow bool, check func() error, emit func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if err := emit(append([]byte(nil), buf[:n]...)); err != nil {
				return err
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
		if !follow {
			return nil
		}
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// leadKeys has the lead sign and refresh for this machine's keys proxy.
type leadKeys struct{ a *Agent }

func (l leadKeys) call(ctx context.Context, method string, params, out any) error {
	sess := l.a.server.Load()
	if sess == nil {
		return errors.New("the lead is unreachable")
	}
	return rpc.Call(ctx, sess, method, params, out)
}

func (l leadKeys) Cert(ctx context.Context, host string, pub []byte) ([]byte, error) {
	var der []byte
	err := l.call(ctx, api.WorkerKeysCert, api.KeysCertParams{Host: host, Public: pub}, &der)
	return der, err
}

func (l leadKeys) Identity(ctx context.Context, box, host string) (keys.Token, error) {
	var t keys.Token
	err := l.call(ctx, api.WorkerKeysIdentity, api.KeysIdentityParams{Box: box, Host: host}, &t)
	return t, err
}

func (l leadKeys) Refresh(ctx context.Context, c keys.RefreshCall) (keys.RefreshAnswer, error) {
	var a keys.RefreshAnswer
	err := l.call(ctx, api.WorkerKeysRefresh, c, &a)
	return a, err
}

// Fence puts every running box to sleep and keeps this machine from acting on
// its own: another machine has its boxes now, or leads in its place.
func (a *Agent) Fence(ctx context.Context, why string) {
	a.fenced.Store(&why)
	bs, err := a.w.List()
	if err != nil {
		log.Printf("fenced: %v", err)
		return
	}
	for _, b := range bs {
		if b.State != worker.Running {
			continue
		}
		if _, err := a.w.Sleep(ctx, b.ID); err != nil {
			log.Printf("fenced: sleep %s: %v", b.ID, err)
		} else {
			log.Printf("fenced: %s asleep", b.ID)
		}
	}
}

// takeLead makes this worker the lead: it keeps the database and master key the
// lead handed over, puts its enrollment aside, and restarts, leading.
func (a *Agent) takeLead(_ context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.TakeLeadParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	state := filepath.Join(a.w.Root, "state")
	if _, err := os.Stat(filepath.Join(state, "hutch.db")); err == nil {
		return nil, fmt.Errorf("%s already holds lead state", state)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	bucket, _ := json.MarshalIndent(p.Bucket, "", "  ")
	for name, data := range map[string][]byte{"hutch.db": p.DB, "master.key": p.Master, "bucket.json": bucket} {
		if err := os.WriteFile(filepath.Join(state, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	aside := filepath.Join(state, "worker-link")
	os.RemoveAll(aside)
	if err := os.Rename(string(a.dir), aside); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(a.w.Root, "fenced"))
	log.Printf("link: taking the lead; restarting to lead")
	if a.Restart != nil {
		time.AfterFunc(time.Second, a.Restart)
	}
	return nil, nil
}
