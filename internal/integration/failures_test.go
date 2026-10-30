//go:build runtime && linux

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
)

func quoteRemote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func remotePython(t *testing.T, script string, args ...string) string {
	t.Helper()
	argv := []string{"python3", "-c", quoteRemote(script)}
	for _, arg := range args {
		argv = append(argv, quoteRemote(arg))
	}
	return remote(t, argv...)
}

func TestCheckpointRecovery(t *testing.T) {
	disposableRuntime(t)
	id := fmt.Sprintf("checkpoint-%d", time.Now().Unix())
	cleanupRuntimeBox(t, id)
	mustCall(t, api.BoxBoot, api.BootParams{ID: id, Image: "base", VCPUs: 1, MemMiB: 1024}, nil)
	counterService(t, id)
	if port(t, id) != "1" {
		t.Fatal("counter did not start")
	}
	remote(t, "hutchd", "box", "checkpoint", id)
	inBox(t, id, "printf live > /workspace/after-checkpoint")
	if port(t, id) != "2" {
		t.Fatal("counter did not advance after its checkpoint")
	}
	remotePython(t, `import http.client,json,pathlib,socket,subprocess,sys
name=sys.argv[1]
root=pathlib.Path('/var/lib/hutch')
box=json.loads((root/'boxes'/name/'box.json').read_text())
partial=root/'boxes'/name/'snapshots'/str(box['generation']+1)
partial.mkdir(exist_ok=True)
(partial/('pages.img' if box.get('gvisor') else 'mem')).write_bytes(b'incomplete checkpoint')
if box.get('gvisor'):
 cmd=[str(root/'gvisor'/box['gvisor']/'runsc'),'--root','/run/hutch/runsc','--overlay2=none','--net-raw','--allow-suid','--allow-connected-on-save','--host-settings=ignore','pause',box.get('runsc_id') or name]
 subprocess.run(cmd,check=True)
else:
 c=http.client.HTTPConnection('localhost')
 c.sock=socket.socket(socket.AF_UNIX)
 c.sock.connect(str(root/'jail'/'firecracker'/name/'root'/'run'/'firecracker.socket'))
 c.request('PATCH','/vm',json.dumps({'state':'Paused'}),{'Content-Type':'application/json'})
 r=c.getresponse()
 assert 200<=r.status<300, (r.status,r.read())
 c.close()
print('Paused with an uncommitted partial snapshot; persisted generation',box['generation'])`, id)
	remote(t, "systemctl", "kill", "--kill-who=main", "--signal=KILL", "hutchd")
	daemonReady(t)
	if port(t, id) != "3" {
		t.Fatal("restart failed to resume paused process memory")
	}
	if inBox(t, id, "cat /workspace/after-checkpoint") != "live" {
		t.Fatal("recovery rolled back live disk writes")
	}
	remote(t, "hutchd", "box", "checkpoint", id)
	t.Log("daemon restart resumed a paused box; subsequent checkpoint replaced the incomplete generation")
	if port(t, id) != "4" {
		t.Fatal("counter did not dirty memory after the checkpoint")
	}

	remotePython(t, `import json,os,pathlib,subprocess,sys
name=sys.argv[1]
root=pathlib.Path('/var/lib/hutch')
meta=root/'boxes'/name/'box.json'
before=json.loads(meta.read_text())['generation']
filler=root/('.checkpoint-fill-'+name)
with open(filler,'xb'): pass
try:
 fs=os.statvfs(root)
 amount=fs.f_bavail*fs.f_frsize-(4<<20)
 assert amount>0
 subprocess.run(['fallocate','-l',str(amount),str(filler)],check=True)
 r=subprocess.run(['/usr/local/bin/hutchd','box','checkpoint',name],capture_output=True,text=True,timeout=50)
 assert r.returncode!=0, 'checkpoint unexpectedly fit on the full filesystem'
 after=json.loads(meta.read_text())['generation']
 assert before==after, ('failed checkpoint replaced the persisted generation',before,after)
 print('Full-disk checkpoint failed; prior generation preserved:',before)
finally:
 filler.unlink(missing_ok=True)`, id)
	if port(t, id) != "5" {
		t.Fatal("failed full-disk checkpoint left the box paused or lost memory")
	}
	remote(t, "hutchd", "box", "checkpoint", id)
	mustCall(t, api.BoxSleep, api.IDParams{ID: id}, nil)
	if port(t, id) != "6" {
		t.Fatal("checkpoint retry did not preserve process memory")
	}
	t.Log("full-disk checkpoint failure resumed the box, retained the prior snapshot, and recovered after space was freed")
}

func TestUpgrade(t *testing.T) {
	if os.Getenv("HUTCH_TEST_UPGRADE") != "1" {
		t.Skip("requires previous-daemon setup")
	}
	disposableRuntime(t)
	files := os.Getenv("HUTCH_TEST_FILES")
	if !filepath.IsAbs(files) {
		t.Fatal("HUTCH_TEST_FILES must name absolute runtime artifacts")
	}
	cli := filepath.Join(files, "hutch")
	versionText, err := exec.Command(cli, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(versionText))
	if len(fields) == 0 {
		t.Fatal("CLI did not report its version")
	}
	want := fields[len(fields)-1]
	var before api.Handshake
	mustCall(t, api.Hello, api.Handshake{Protocol: api.Protocol, Version: "upgrade test"}, &before)
	if before.Version == want {
		t.Fatal("upgrade test did not start from the previous daemon")
	}
	id := fmt.Sprintf("upgrade-%d", time.Now().Unix())
	cleanupRuntimeBox(t, id)
	mustCall(t, api.BoxBoot, api.BootParams{ID: id, Image: "base", VCPUs: 1, MemMiB: 1024}, nil)
	counterService(t, id)
	if port(t, id) != "1" {
		t.Fatal("counter did not start")
	}
	pid := remote(t, "systemctl", "show", "--property=MainPID", "--value", "hutchd")
	boxPID := inBox(t, id, "cat /tmp/counter.pid")
	candidate := "/var/lib/hutch/.upgrade-candidate-" + id
	remotePython(t, `import pathlib,shutil,sys
shutil.copyfile(sys.argv[1],sys.argv[2])
pathlib.Path(sys.argv[2]).chmod(0o755)`, filepath.Join(files, "hutchd"), candidate)
	t.Cleanup(func() {
		remotePython(t, `import pathlib,sys
pathlib.Path(sys.argv[1]).unlink(missing_ok=True)`, candidate)
	})
	t.Cleanup(func() {
		if t.Failed() {
			remotePython(t, `import os,pathlib,shutil,sys
tmp='/usr/local/bin/hutchd.test-repair'
shutil.copyfile(sys.argv[1],tmp)
pathlib.Path(tmp).chmod(0o755)
os.replace(tmp,'/usr/local/bin/hutchd')`, filepath.Join(files, "hutchd"))
		}
	})
	shim := filepath.Join(t.TempDir(), "hutchd-failing-checkpoint")
	if err := os.Symlink(filepath.Join(files, "hutch-guest"), filepath.Join(filepath.Dir(shim), "hutch-guest")); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"${1:-}\" = box ] && [ \"${2:-}\" = checkpoint ]; then echo 'injected checkpoint failure' >&2; exit 74; fi\nexec " + quoteRemote(candidate) + " \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	upgrade := func(binary string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, cli, "machine", "upgrade", "--hutchd", binary, "--images", files)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	output, err := upgrade(shim)
	if err == nil || !strings.Contains(output, "injected checkpoint failure") {
		t.Fatalf("injected upgrade did not fail at checkpoint: %v\n%s", err, output)
	}
	if got := remote(t, "systemctl", "show", "--property=MainPID", "--value", "hutchd"); got != pid {
		t.Fatal("failed checkpoint restarted the old daemon")
	}
	var interrupted api.Handshake
	mustCall(t, api.Hello, api.Handshake{Protocol: api.Protocol, Version: "upgrade test"}, &interrupted)
	if interrupted.Version != before.Version || !strings.HasSuffix(remote(t, "hutchd", "version"), want) {
		t.Fatal("did not reproduce a new installed binary with an old running daemon")
	}
	if port(t, id) != "2" {
		t.Fatal("aborted upgrade lost running work")
	}
	t.Log("checkpoint failure prevented restart; installed binary reports the new version while the old daemon and box remain running")
	output, err = upgrade(filepath.Join(files, "hutchd")) // No --force: the running version must prevent a false no-op.
	if err != nil {
		t.Fatalf("upgrade retry failed: %v\n%s", err, output)
	}
	var after api.Handshake
	mustCall(t, api.Hello, api.Handshake{Protocol: api.Protocol, Version: "upgrade test"}, &after)
	if after.Version != want || remote(t, "systemctl", "show", "--property=MainPID", "--value", "hutchd") == pid {
		t.Fatal("retry did not start the new daemon")
	}
	if port(t, id) != "3" || inBox(t, id, "cat /tmp/counter.pid") != boxPID {
		t.Fatal("successful upgrade lost the live box's process state")
	}
	t.Logf("actual previous daemon %s upgraded to %s; retry detected the interrupted install and retained the live process", before.Version, after.Version)
}
