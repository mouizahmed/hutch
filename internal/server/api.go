package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

func (s *Server) apiMux() rpc.Mux {
	return rpc.Mux{
		api.BoxList:          s.boxList,
		api.BoxBoot:          s.boxBoot,
		api.BoxFork:          s.boxFork,
		api.BoxRelease:       s.boxRelease,
		api.ProjectList:      s.projectList,
		api.BoxSleep:         s.onBoxWorker(api.BoxSleep),
		api.BoxWake:          s.boxWake,
		api.BoxRm:            s.boxRm,
		api.BoxExec:          s.boxExec,
		api.BoxConsole:       s.boxConsole,
		api.BoxStatus:        s.boxStatus,
		api.ThreadNew:        s.threadNew,
		api.ThreadRm:         s.threadRm,
		api.ThreadPin:        s.threadPin,
		api.ThreadRename:     s.threadRename,
		api.ThreadResume:     s.threadResume,
		api.ThreadSend:       s.threadSend,
		api.ConfigGet:        s.configGet,
		api.UserGet:          s.userGet,
		api.UserPutConfig:    s.userPutConfig,
		api.UserLoginBox:     s.userLoginBox,
		api.UserCapture:      s.userCapture,
		api.ProjectInit:      s.projectInit,
		api.TemplateBuild:    s.templateBuild,
		api.TemplateSnapshot: s.templateSnapshot,
		api.TemplateList:     s.templateList,
		api.TemplateBox:      s.templateBox,
		api.ProjectConfirm:   s.projectConfirm,
		api.ProjectRm:        s.projectRm,
		api.SecretsGet:       s.secretsGet,
		api.SecretsPut:       s.secretsPut,
		api.ConfigPut:        s.configPut,
		api.ServerInit:       s.serverInit,
		api.ServerGC:         s.serverGC,
		api.BoxMove:          s.boxMove,
		api.BoxResize:        s.boxResize,
		api.BoxPlan:          s.boxPlan,
		api.WorkerDrain:      s.workerDrain,
		api.WorkerRm:         s.workerRm,
		api.BucketGet:        s.bucketGet,
		api.Hello:            s.hello,
		api.NotifyTest:       s.notifyTest,
		api.Doctor:           s.doctor,
		api.WorkerList:       s.workerList,
	}
}

func (s *Server) boxList(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	threads, err := s.store.threads()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range boxes {
		ts := threads[boxes[i].ID]
		for j := range ts {
			st := s.threadStates[boxes[i].ID][ts[j].N]
			if boxes[i].State == "running" {
				ts[j].State = st.State
			}
			if st.Title != "" && !ts[j].Renamed {
				ts[j].Title = st.Title
			}
			ts[j].Tokens = st.Tokens
		}
		boxes[i].Threads = ts
		boxes[i].PushError = s.pushErrors[boxes[i].ID]
	}
	return boxes, nil
}

func (s *Server) workerList(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	boxes, err := s.store.boxes()
	if err != nil {
		return nil, err
	}
	reserved := map[string]int{}
	for _, b := range boxes {
		if b.State == "running" {
			reserved[b.Worker] += b.MemMiB
		}
	}
	s.mu.Lock()
	for i := range ws {
		ws[i].ReservedMiB = reserved[ws[i].ID]
		if w := s.workers[ws[i].ID]; w != nil {
			ws[i].Online, ws[i].Disk = true, w.info.Disk
		}
	}
	s.mu.Unlock()
	return ws, nil
}

// callForBox sends a box call to a worker and records the box it returns.
func (s *Server) callForBox(ctx context.Context, w *workerConn, method string, params any) (api.Box, error) {
	var b api.Box
	if err := rpc.Call(ctx, w.sess, method, params, &b); err != nil {
		return b, err
	}
	b.Worker = w.id
	if err := s.store.putBox(b); err != nil {
		return b, err
	}
	s.store.event(b.ID, method, b.State)
	return b, nil
}

func (s *Server) boxBoot(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.BootParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := s.store.box(p.ID); err == nil {
		return nil, fmt.Errorf("box %q already exists", p.ID)
	}
	n := need{memMiB: p.MemMiB, vcpus: p.VCPUs}
	if err := s.checkFits(ctx, n); err != nil {
		return nil, err
	}
	_, stderr := out.Writers()
	w, release, err := s.placeWait(ctx, n, p.Worker, p.ID, stderr)
	if err != nil {
		return nil, err
	}
	defer release()
	b, err := s.callForBox(ctx, w, api.BoxBoot, p)
	if err != nil {
		// The worker keeps a box that failed to boot so its console can be read.
		s.store.putBox(api.Box{ID: p.ID, Worker: w.id, State: "failed", Image: p.Image, VCPUs: p.VCPUs, MemMiB: p.MemMiB, Created: time.Now().UTC()})
		s.store.event(p.ID, api.BoxBoot, "failed: "+err.Error())
		return nil, err
	}
	return b, nil
}

func (s *Server) boxFork(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.ForkParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := s.store.box(p.ID); err == nil {
		return nil, fmt.Errorf("box %q already exists", p.ID)
	}
	if p.Project != "" {
		stdout, stderr := out.Writers()
		return s.forkProject(ctx, p, stdout, stderr)
	}
	src, err := s.store.box(p.Source)
	if err != nil {
		return nil, err
	}
	w, err := s.worker(src.Worker)
	if err != nil {
		return nil, err
	}
	b, err := s.callForBox(ctx, w, api.BoxFork, p)
	if err != nil {
		return nil, err
	}
	_, stderr := out.Writers()
	if err := s.linkPath(ctx, w, b.ID, p.Path, stderr); err != nil {
		return b, err
	}
	if err := s.applyUserLayer(ctx, w, b.ID, p.Credentials); err != nil {
		return b, err
	}
	if p.Delta.Size() > 0 {
		return b, s.extractFiles(ctx, w, b.ID, p.Delta, stderr)
	}
	return b, nil
}

func (s *Server) workerFor(id string) (*workerConn, error) {
	b, err := s.store.box(id)
	if err != nil {
		return nil, err
	}
	return s.worker(b.Worker)
}

func (s *Server) onBoxWorker(method string) rpc.Handler {
	return func(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
		var p api.IDParams
		if err := rpc.Decode(params, &p); err != nil {
			return nil, err
		}
		b, err := s.store.box(p.ID)
		if err != nil {
			return nil, err
		}
		if err := notVersion(b); err != nil {
			return nil, err
		}
		w, err := s.worker(b.Worker)
		if err != nil {
			return nil, err
		}
		return s.callForBox(ctx, w, method, p)
	}
}

func (s *Server) boxWake(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	b, err := s.store.box(p.ID)
	if err != nil {
		return nil, err
	}
	if b.State != "asleep" && b.State != "archived" {
		if _, err := s.worker(b.Worker); err == nil {
			return nil, fmt.Errorf("box %q is %s", p.ID, b.State)
		}
	}
	if _, err := s.awake(ctx, p.ID); err != nil {
		return nil, err
	}
	return s.store.box(p.ID)
}

// notVersion refuses to touch a template version: boxes fork from it as it is.
func notVersion(b api.Box) error {
	if b.Kind == api.BoxVersion {
		return fmt.Errorf("%s is a frozen template version; use hutch template shell", b.ID)
	}
	return nil
}

func (s *Server) boxRm(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	b, err := s.store.box(p.ID)
	if err != nil {
		return nil, err
	}
	if err := notVersion(b); err != nil {
		return nil, err
	}
	return nil, s.removeBox(ctx, p.ID)
}

// awake returns the worker of a box, waking the box first if it is asleep.
func (s *Server) awake(ctx context.Context, id string) (*workerConn, error) {
	b, err := s.store.box(id)
	if err != nil {
		return nil, err
	}
	if err := notVersion(b); err != nil {
		return nil, err
	}
	if b.State == "asleep" || b.State == "archived" {
		release, err := s.admitWake(ctx, id)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	w, b, err := s.boxWorker(ctx, b)
	if err != nil {
		return nil, err
	}
	if b.State == "asleep" || b.State == "archived" {
		if _, err := s.callForBox(ctx, w, api.BoxWake, api.IDParams{ID: id}); err != nil {
			// A failed wake leaves the box as it was; a report taken while it was
			// starting may have recorded it running.
			s.store.putBox(b)
			return nil, fmt.Errorf("wake %s: %w", id, err)
		}
		s.refreshCredentials(ctx, w, id)
		if b.State == "archived" {
			s.resumeThreads(ctx, w, id)
		}
	}
	return w, nil
}

func (s *Server) boxExec(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.ExecParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	w, err := s.awake(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return rpc.Relay(ctx, w.sess, api.BoxExec, params, out)
}

func (s *Server) boxConsole(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.ConsoleParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	w, err := s.workerFor(p.ID)
	if err != nil {
		return nil, err
	}
	return rpc.Relay(ctx, w.sess, api.BoxConsole, params, out)
}

func (s *Server) apiStreams() rpc.Streams {
	return rpc.Streams{api.BoxPTY: s.boxPTY, api.BoxSerial: s.boxSerial, api.BoxDial: s.boxDial, api.BoxExecIn: s.boxExecIn}
}

func (s *Server) boxDial(ctx context.Context, params json.RawMessage, conn net.Conn) error {
	var p api.DialParams
	if err := rpc.Decode(params, &p); err != nil {
		return err
	}
	w, err := s.workerFor(p.ID)
	if err != nil {
		return err
	}
	wc, err := rpc.OpenStream(ctx, w.sess, api.BoxDial, p)
	if err != nil {
		return err
	}
	rpc.Splice(conn, wc)
	return nil
}

func (s *Server) boxStatus(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	w, err := s.workerFor(p.ID)
	if err != nil {
		return nil, err
	}
	return rpc.Relay(ctx, w.sess, api.BoxStatus, params, out)
}

// boxSerial does not wake the box: the serial console is for boxes whose guest is broken.
func (s *Server) boxSerial(ctx context.Context, params json.RawMessage, conn net.Conn) error {
	var p api.ConsoleParams
	if err := rpc.Decode(params, &p); err != nil {
		return err
	}
	w, err := s.workerFor(p.ID)
	if err != nil {
		return err
	}
	wc, err := rpc.OpenStream(ctx, w.sess, api.BoxSerial, p)
	if err != nil {
		return err
	}
	s.store.event(p.ID, api.BoxSerial, "")
	rpc.Splice(conn, wc)
	return nil
}

func (s *Server) boxExecIn(ctx context.Context, params json.RawMessage, conn net.Conn) error {
	var p api.ExecParams
	if err := rpc.Decode(params, &p); err != nil {
		return err
	}
	w, err := s.awake(ctx, p.ID)
	if err != nil {
		return err
	}
	wc, err := rpc.OpenStream(ctx, w.sess, api.BoxExecIn, p)
	if err != nil {
		return err
	}
	s.store.event(p.ID, api.BoxExec, strings.Join(p.Argv, " "))
	rpc.Splice(conn, wc)
	return nil
}

func (s *Server) boxPTY(ctx context.Context, params json.RawMessage, conn net.Conn) error {
	var p api.PTYParams
	if err := rpc.Decode(params, &p); err != nil {
		return err
	}
	w, err := s.awake(ctx, p.ID)
	if err != nil {
		return err
	}
	wc, err := rpc.OpenStream(ctx, w.sess, api.BoxPTY, p)
	if err != nil {
		return err
	}
	s.store.event(p.ID, api.BoxPTY, strings.Join(p.Argv, " "))
	rpc.Splice(conn, wc)
	return nil
}
