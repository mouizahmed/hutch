//go:build linux

package agent

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/link"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

// A worker connected to its lead never acts on the CLI's calls: a worker taken
// over could otherwise pass them on with the lead's authority, or act on boxes
// the lead thinks it controls.
func TestWorkerRefusesCLIWhileLinked(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	lead, err := yamux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := &Agent{mux: rpc.Mux{}, streams: rpc.Streams{}}
	ag.server.Store(lead)

	dir := t.TempDir()
	socket := filepath.Join(dir, "api.sock")
	go ag.ServeCLI(ctx, socket, link.Dir(dir))
	var conn net.Conn
	for i := 0; ; i++ {
		if conn, err = net.Dial("unix", socket); err == nil {
			break
		}
		if i > 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess, err := yamux.Client(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	refused := func(err error) bool { return err != nil && strings.Contains(err.Error(), "this machine is a worker") }
	for _, m := range []string{api.Hello, api.BoxList, api.BoxExec, api.BoxSleep, api.BoxWake, api.BoxConsole, api.BoxStatus} {
		if err := rpc.Call(ctx, sess, m, api.IDParams{ID: "box"}, nil); !refused(err) {
			t.Errorf("%s: %v", m, err)
		}
	}
	for _, m := range []string{api.BoxPTY, api.BoxExecIn, api.BoxDial, api.BoxSerial} {
		c, err := rpc.OpenStream(ctx, sess, m, api.IDParams{ID: "box"})
		if err != nil {
			t.Fatal(err)
		}
		var f rpc.Frame
		if err := wire.Read(c, &f); err != nil || f.Type != rpc.FrameError || !strings.Contains(f.Error, "this machine is a worker") {
			t.Errorf("%s: %+v %v", m, f, err)
		}
		c.Close()
	}
	if err := rpc.Call(ctx, sess, api.BoxFork, api.ForkParams{ID: "x"}, nil); err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Errorf("a lead-only call: %v", err)
	}
	// A fenced worker refuses too, with its lead gone.
	ag.server.Store(nil)
	why := "another machine took over its boxes"
	ag.fenced.Store(&why)
	if err := rpc.Call(ctx, sess, api.BoxWake, api.IDParams{ID: "box"}, nil); err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Errorf("a fenced worker's wake: %v", err)
	}
}
