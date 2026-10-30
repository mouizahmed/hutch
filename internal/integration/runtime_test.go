//go:build runtime && linux

// Package integration exercises real boxes on an explicitly disposable machine.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/client"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

func call(ctx context.Context, method string, params, result any) error {
	sess, err := client.Dial(os.Getenv("HUTCH_MACHINE"), client.Socket)
	if err != nil {
		return err
	}
	defer sess.Close()
	return rpc.Call(ctx, sess, method, params, result)
}

func mustCall(t *testing.T, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if err := call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func inBox(t *testing.T, id, script string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	sess, err := client.Dial(os.Getenv("HUTCH_MACHINE"), client.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var out, errout bytes.Buffer
	argv := append([]string{"sh", "-c", "set -eu\n" + script, "sh"}, args...)
	code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: id, Argv: argv}, &out, &errout)
	if err != nil || code != 0 {
		t.Fatalf("exec %s: code=%d err=%v stdout=%s stderr=%s", id, code, err, out.String(), errout.String())
	}
	return strings.TrimSpace(out.String())
}

func port(t *testing.T, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	sess, err := client.Dial(os.Getenv("HUTCH_MACHINE"), client.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	conn, err := rpc.OpenStream(ctx, sess, api.BoxDial, api.DialParams{ID: id, Port: 19555})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var ready guestproto.Response
	if err := wire.Read(conn, &ready); err != nil || ready.Type != guestproto.RespOK {
		t.Fatalf("port handshake: %+v, %v", ready, err)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func remote(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	argv := append([]string{"-o", "BatchMode=yes", os.Getenv("HUTCH_MACHINE"), "sudo", "-n"}, args...)
	out, err := exec.CommandContext(ctx, "ssh", argv...).CombinedOutput()
	if err != nil {
		t.Fatalf("remote %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func disposableRuntime(t *testing.T) {
	t.Helper()
	if os.Getenv("HUTCH_TEST_DISPOSABLE") != "1" {
		t.Skip("requires an explicitly disposable machine")
	}
	backend := os.Getenv("HUTCH_TEST_BACKEND")
	if backend != "firecracker" && backend != "gvisor" {
		t.Fatal("set HUTCH_TEST_BACKEND to firecracker or gvisor")
	}
	var workers []api.Worker
	mustCall(t, api.WorkerList, struct{}{}, &workers)
	if len(workers) != 1 || !strings.Contains(workers[0].Class, map[string]string{"firecracker": "/fc-", "gvisor": "/gvisor-"}[backend]) {
		t.Fatalf("expected actual %s backend, got %+v", backend, workers)
	}
}

func counterService(t *testing.T, id string) {
	t.Helper()
	inBox(t, id, `nohup node -e 'let n=0;require("net").createServer(s=>s.on("data",()=>s.write(String(++n)))).listen(19555,"0.0.0.0")' >/tmp/counter.log 2>&1 </dev/null &
i=0; until ss -ltn | grep -q ':19555'; do i=$((i+1)); test "$i" -lt 100 || exit 1; sleep 0.1; done`)
}

func daemonReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		var workers []api.Worker
		err := call(ctx, api.WorkerList, struct{}{}, &workers)
		cancel()
		if err == nil && len(workers) == 1 && workers[0].Online {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not recover after being killed")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func cleanupRuntimeBox(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			for _, path := range []string{"/var/lib/hutch/boxes/" + id + "/console.log", "/sys/fs/cgroup/hutch/" + id + "/memory.events"} {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				out, _ := exec.CommandContext(ctx, "ssh", os.Getenv("HUTCH_MACHINE"), "sudo", "tail", "-n", "60", path).CombinedOutput()
				cancel()
				t.Logf("%s: %s", path, out)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := call(ctx, api.BoxRm, api.IDParams{ID: id}, nil); err != nil {
			exec.CommandContext(ctx, "ssh", os.Getenv("HUTCH_MACHINE"), "sudo", "-n", "hutchd", "box", "rm", id).Run()
		}
	})
}

func TestRuntime(t *testing.T) {
	disposableRuntime(t)
	parent := fmt.Sprintf("runtime-%d", time.Now().Unix())
	child := parent + "-child"
	for _, id := range []string{parent, child} {
		cleanupRuntimeBox(t, id)
	}
	var original api.Box
	mustCall(t, api.BoxBoot, api.BootParams{ID: parent, Image: "base", VCPUs: 1, MemMiB: 1024}, &original)
	inBox(t, parent, "printf original > /workspace/marker")
	counterService(t, parent)
	if got := port(t, parent); got != "1" {
		t.Fatalf("initial counter=%q", got)
	}
	mustCall(t, api.BoxSleep, api.IDParams{ID: parent}, nil)
	var clone api.Box
	mustCall(t, api.BoxFork, api.ForkParams{Source: parent, ID: child, Credentials: []string{"*"}}, &clone)
	if got := port(t, child); got != "2" {
		t.Fatalf("clone lost its process memory: %q", got)
	}
	inBox(t, child, `printf child > /workspace/marker`)
	mustCall(t, api.BoxWake, api.IDParams{ID: parent}, &original)
	if got := port(t, parent); got != "2" {
		t.Fatalf("parent lost its process memory: %q", got)
	}
	if got := inBox(t, parent, "cat /workspace/marker"); got != "original" {
		t.Fatalf("clone wrote into parent: %q", got)
	}
	parentID := inBox(t, parent, "cat /etc/machine-id")
	childID := inBox(t, child, "cat /etc/machine-id")
	if parentID == "" || childID == "" || parentID == childID {
		t.Fatal("fork did not assign an independent machine identity")
	}
	t.Log("cold boot, warm sleep/wake, fork memory, separate disks, and identity passed")

	sentinel := filepath.Join("/tmp", parent+"-host-only")
	remote(t, "touch", sentinel)
	t.Cleanup(func() { exec.Command("ssh", os.Getenv("HUTCH_MACHINE"), "sudo", "rm", "-f", sentinel).Run() })
	hostIP := strings.Fields(remote(t, "hostname", "-I"))[0]
	inBox(t, child, `test ! -e "$1"
node -e 'const net=require("net"); (async()=>{for(const target of process.argv.slice(1)){const [host,port]=target.split(":");await new Promise((resolve,reject)=>{const s=net.connect({host,port:Number(port)});s.setTimeout(1200);s.on("error",()=>resolve());s.on("timeout",()=>{s.destroy();resolve()});s.on("connect",()=>{s.destroy();reject(Error("reachable: "+target))})})}})().catch(e=>{console.error(e.message);process.exit(12)})' 169.254.169.254:80 169.254.0.21:22 10.0.0.1:22 "$2" "$3"
test -n "$HTTPS_PROXY"
status=$(curl --proxy "$HTTPS_PROXY" --noproxy '' -sS --connect-timeout 1 --max-time 2 -o /tmp/proxy-denied -w '%{http_connect}' https://127.0.0.1:22 2>/dev/null || true)
test "$status" = 502 || { echo "expected proxy refusal, got $status" >&2; exit 1; }`, sentinel, net.JoinHostPort(original.ReachIP, "19555"), net.JoinHostPort(hostIP, "22"))
	t.Log("host files, metadata, gateway SSH, private network, lateral access, and proxy loopback refused")

	remote(t, "systemctl", "kill", "--kill-who=main", "--signal=KILL", "hutchd")
	daemonReady(t)
	if got := port(t, parent); got != "3" {
		t.Fatalf("daemon restart lost warm process state: %q", got)
	}
	t.Log("daemon crash/restart preserved the box's process memory")

	mustCall(t, api.BoxSleep, api.IDParams{ID: child}, nil)
	remote(t, "hutchd", "box", "archive", child)
	mustCall(t, api.BoxWake, api.IDParams{ID: child}, nil)
	if got := inBox(t, child, "cat /workspace/marker"); got != "child" {
		t.Fatalf("cold archive/wake lost disk writes: %q", got)
	}
	if got := inBox(t, child, "cat /etc/machine-id"); got != childID {
		t.Fatal("cold boot changed this box's identity")
	}
	t.Log("archive and cold boot preserved files and box identity")
}
