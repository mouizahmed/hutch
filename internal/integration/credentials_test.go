//go:build runtime && linux

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
)

func TestCredentialIsolation(t *testing.T) {
	disposableRuntime(t)
	var before api.UserState
	mustCall(t, api.UserGet, struct{}{}, &before)
	if len(before.Credentials) != 0 || len(before.Layer.Adapters) != 0 || len(before.Layer.Files) != 0 {
		t.Fatal("credential fixtures require an empty disposable user layer")
	}
	t.Cleanup(func() {
		var current api.UserState
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if call(ctx, api.UserGet, struct{}{}, &current) == nil {
			if err := call(ctx, api.UserPutConfig, api.UserPutConfigParams{BaseVersion: current.Version, Layer: before.Layer}, nil); err != nil {
				t.Error(err)
			}
		}
	})
	project := "github.com/mouizahmed/hutch"
	var projects []api.Project
	mustCall(t, api.ProjectList, struct{}{}, &projects)
	if len(projects) != 0 {
		t.Fatal("credential fixtures require no existing projects")
	}
	mustCall(t, api.ProjectInit, api.ProjectParams{Project: project}, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := call(ctx, api.ProjectRm, api.ProjectParams{Project: project}, nil); err != nil {
			t.Error(err)
		}
	})
	layer := api.UserLayer{
		Adapters: map[string]string{
			"claude.toml": "name=\"claude\"\nstart=\"true\"\nlogin=\"true\"\nhosts=[\"credential.invalid\"]\ncredentials=[{file=\"~/.claude/.credentials.json\",keys=[\"claudeAiOauth\"],secrets=[\"accessToken\",\"refreshToken\"]}]\n",
			"codex.toml":  "name=\"codex\"\nstart=\"true\"\n",
			"phase9.toml": "name=\"phase9\"\nstart=\"sh\"\nconfig=[\"~/.phase9.json\"]\n[mcp]\nfile=\"~/.phase9.json\"\nkey=\"mcpServers\"\nsecrets=[\"env\"]\n",
		},
		Files: map[string][]guestproto.File{"phase9": {{Path: "~/.phase9.json", Data: []byte(`{"mcpServers":{"probe":{"url":"https://credential.invalid/mcp"}}}`)}}},
	}
	marker := func() string {
		var raw [24]byte
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatal(err)
		}
		return "sk-phase9-" + hex.EncodeToString(raw[:])
	}
	first, rotated := marker(), marker()
	put := func(base int64, token string) api.UserState {
		data, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"probe": map[string]any{"env": map[string]string{"TOKEN": token}}}})
		if err != nil {
			t.Fatal(err)
		}
		var state api.UserState
		mustCall(t, api.UserPutConfig, api.UserPutConfigParams{BaseVersion: base, Layer: layer,
			Secrets: map[string][]guestproto.File{"phase9": {{Path: "~/.phase9.json", Data: data, Merge: true}}}}, &state)
		return state
	}
	state := put(before.Version, first)
	var source api.Box
	mustCall(t, api.TemplateBuild, api.TemplateParams{Project: project, CloneURL: "https://github.com/mouizahmed/hutch.git",
		Commit: "375eb5dd625f53879edd83facc0280ccf5ce5705", VCPUs: 1, MemMiB: 1024, Credentials: []string{}, BuildTimeout: 3 * time.Minute}, &source)
	mustCall(t, api.BoxSleep, api.IDParams{ID: source.ID}, nil)
	plain, granted := fmt.Sprintf("credential-none-%d", time.Now().Unix()), fmt.Sprintf("credential-granted-%d", time.Now().Unix())
	for _, id := range []string{plain, granted} {
		cleanupRuntimeBox(t, id)
	}
	inspect := func(id string, hasGrant bool) {
		t.Helper()
		config := inBox(t, id, "cat ~/.phase9.json")
		env := inBox(t, id, "env")
		if strings.Contains(config+env, first) || strings.Contains(config+env, rotated) {
			t.Fatal("raw synthetic key reached box files or environment")
		}
		var data struct {
			Servers map[string]struct {
				Env map[string]string `json:"env"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(config), &data); err != nil {
			t.Fatal(err)
		}
		token := data.Servers["probe"].Env["TOKEN"]
		if hasGrant && !strings.Contains(token, "hutch") {
			t.Fatal("granted box did not receive a placeholder in its MCP config")
		}
		if !hasGrant && token != "" {
			t.Fatal("box without grants received an MCP credential")
		}
	}
	for id, creds := range map[string][]string{plain: {}, granted: {"phase9"}} {
		mustCall(t, api.BoxFork, api.ForkParams{Source: source.ID, ID: id, Credentials: creds}, nil)
		inspect(id, id == granted)
		mustCall(t, api.BoxSleep, api.IDParams{ID: id}, nil)
	}
	put(state.Version, rotated)
	cleanupRuntimeBox(t, granted+"-narrow")
	var err error
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	err = call(ctx, api.BoxFork, api.ForkParams{Source: granted, ID: granted + "-narrow", Credentials: []string{}}, nil)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "not granted") {
		t.Fatalf("snapshot grants were improperly narrowed: %v", err)
	}
	remotePython(t, `import errno,json,os,pathlib,sys
root=pathlib.Path('/var/lib/hutch/boxes')
markers=[x.encode() for x in sys.argv[1:3]]
for name in sys.argv[3:]:
 box=json.loads((root/name/'box.json').read_text())
 gen=root/name/'snapshots'/str(box['generation'])
 scanned=0
 scanned_bytes=0
 for path in gen.iterdir():
  if not path.is_file(): continue
  with open(path,'rb',buffering=0) as f:
   end=os.fstat(f.fileno()).st_size
   pos=0
   while pos<end:
    try: start=os.lseek(f.fileno(),pos,os.SEEK_DATA)
    except OSError as e:
     if e.errno==errno.ENXIO: break
     raise
    stop=os.lseek(f.fileno(),start,os.SEEK_HOLE)
    f.seek(start)
    tail=b''
    while f.tell()<stop:
     block=f.read(min(1<<20,stop-f.tell()))
     if not block: break
     scanned_bytes+=len(block)
     data=tail+block
     assert all(m not in data for m in markers), 'raw synthetic key found in guest snapshot'
     tail=data[-128:]
    pos=stop
  scanned+=1
 assert scanned>0 and scanned_bytes>0, 'no snapshot data was scanned'
 print(name,'snapshot files without raw markers:',scanned,'bytes:',scanned_bytes)`, first, rotated, source.ID, plain, granted)
	mustCall(t, api.BoxWake, api.IDParams{ID: granted}, nil)
	inspect(granted, true)
	t.Log("custom-adapter MCP grants used placeholders; raw synthetic keys absent from guest files, environment and snapshot bytes; narrowing refused; rotation/wake preserved isolation")

	// Use Claude's actual shared-file shape without a provider/model request.
	// Capturing and refreshing its managed login must preserve box-local MCP OAuth.
	claude := fmt.Sprintf("credential-oauth-%d", time.Now().Unix())
	cleanupRuntimeBox(t, claude)
	mustCall(t, api.BoxFork, api.ForkParams{Source: source.ID, ID: claude, Credentials: []string{"claude"}}, nil)
	login, native := marker(), marker()
	inBox(t, claude, `mkdir -p ~/.claude
node -e 'require("fs").writeFileSync(process.env.HOME+"/.claude/.credentials.json",JSON.stringify({claudeAiOauth:{accessToken:process.argv[1]},mcpOAuth:{fixture:{accessToken:process.argv[2]}}}))' "$1" "$2"`, login, native)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			login = marker()
			inBox(t, claude, `node -e 'const f=process.env.HOME+"/.claude/.credentials.json",fs=require("fs"),d=JSON.parse(fs.readFileSync(f));d.claudeAiOauth.accessToken=process.argv[1];fs.writeFileSync(f,JSON.stringify(d))' "$1"`, login)
		}
		mustCall(t, api.UserCapture, api.CaptureParams{Box: claude, Adapter: "claude"}, nil)
		mustCall(t, api.BoxSleep, api.IDParams{ID: claude}, nil)
		mustCall(t, api.BoxWake, api.IDParams{ID: claude}, nil)
		got := inBox(t, claude, "cat ~/.claude/.credentials.json")
		if strings.Contains(got, login) || !strings.Contains(got, native) || !strings.Contains(got, "hutch") {
			t.Fatal("managed login capture or wake lost native MCP OAuth or exposed the raw agent login")
		}
	}
	t.Log("Claude-format MCP OAuth survived two managed login captures and warm sleep/wake cycles")
}
