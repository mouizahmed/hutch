package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

const credentialsPrefix = "credentials/"

func (s *Server) userLayer() (api.UserLayer, int64, error) {
	var l api.UserLayer
	p, err := s.store.layerPart("config")
	if err != nil || p.version == 0 {
		return l, p.version, err
	}
	return l, p.version, json.Unmarshal(p.content, &l)
}

// adapters is the built-in adapters overridden by the user's own.
func (s *Server) adapters() (agents.Set, error) {
	l, _, err := s.userLayer()
	if err != nil {
		return nil, err
	}
	return agents.Load(l.Adapters)
}

func (s *Server) credentials() ([]api.Credentials, error) {
	parts, err := s.store.layerParts(credentialsPrefix)
	if err != nil {
		return nil, err
	}
	var out []api.Credentials
	for name, p := range parts {
		c := api.Credentials{Part: strings.TrimPrefix(name, credentialsPrefix), Version: p.version, Updated: p.updated}
		if err := json.Unmarshal(p.content, &c.Files); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b api.Credentials) int { return strings.Compare(a.Part, b.Part) })
	return out, nil
}

func (s *Server) userGet(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	l, v, err := s.userLayer()
	if err != nil {
		return nil, err
	}
	creds, err := s.credentials()
	if err != nil {
		return nil, err
	}
	for i := range creds {
		for j := range creds[i].Files {
			creds[i].Files[j].Data = nil
		}
	}
	return api.UserState{Version: v, Layer: l, Credentials: creds}, nil
}

func (s *Server) userPutConfig(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.UserPutConfigParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := agents.Load(p.Layer.Adapters); err != nil {
		return nil, err
	}
	data, err := json.Marshal(p.Layer)
	if err != nil {
		return nil, err
	}
	v, conflict, err := s.store.putLayerPart("config", p.BaseVersion, data)
	if err != nil {
		return nil, err
	}
	if conflict {
		return nil, errors.New("the user layer changed since you looked; run hutch user sync again")
	}
	if p.KeepSecrets {
		return api.UserState{Version: v, Layer: p.Layer}, nil
	}
	existing, err := s.store.layerParts(credentialsPrefix)
	if err != nil {
		return nil, err
	}
	for name := range existing {
		if a, ok := strings.CutSuffix(strings.TrimPrefix(name, credentialsPrefix), "/mcp"); ok && p.Secrets[a] == nil {
			s.store.deleteLayerPart(name)
		}
	}
	for a, files := range p.Secrets {
		data, _ := json.Marshal(files)
		if _, _, err := s.store.putLayerPart(credentialsPrefix+a+"/mcp", -1, data); err != nil {
			return nil, err
		}
	}
	return api.UserState{Version: v, Layer: p.Layer}, nil
}

// granted reports whether a box with these credentials grants gets adapter a's logins.
func granted(creds []string, a string) bool {
	return slices.Contains(creds, "*") || slices.Contains(creds, a)
}

// applyUserLayer writes the user layer into a running box: config, the granted
// credentials, then each agent's first-run seeds, then git identity and gh's git
// credential helper.
func (s *Server) applyUserLayer(ctx context.Context, w *workerConn, box string, creds []string) error {
	l, _, err := s.userLayer()
	if err != nil {
		return err
	}
	set, err := agents.Load(l.Adapters)
	if err != nil {
		return err
	}
	stored, err := s.credentials()
	if err != nil {
		return err
	}
	if err := s.store.setBoxCredentials(box, creds); err != nil {
		return err
	}
	var files []guestproto.File
	var watch []string
	for _, a := range set.Sorted() {
		files = append(files, l.Files[a.Name]...)
		if granted(creds, a.Name) {
			for _, src := range a.Credentials {
				watch = append(watch, src.File)
			}
		}
	}
	hasGH := false
	for _, c := range stored {
		a, _, _ := strings.Cut(c.Part, "/")
		if granted(creds, a) {
			files = append(files, c.Files...)
			hasGH = hasGH || c.Part == "gh"
		}
	}
	dirs := []string{workspace}
	if b, err := s.store.box(box); err == nil && b.Path != "" {
		dirs = append(dirs, b.Path)
	}
	for _, a := range set.Sorted() {
		for _, seed := range a.Seed {
			data, err := seedData(seed, dirs)
			if err != nil {
				return fmt.Errorf("adapter %s: seed %s: %w", a.Name, seed.File, err)
			}
			files = append(files, guestproto.File{Path: seed.File, Mode: 0o600, Data: data, Merge: true})
		}
	}
	// Guest frames are limited to 4 MB, and file data travels base64-encoded (a third
	// larger), so batches stay under 2 MB of data. The last call also starts watching
	// credentials.
	for first := true; first || len(files) > 0; first = false {
		n, size := 0, 0
		for n < len(files) && (n == 0 || size+len(files[n].Data) < 2<<20) {
			size += len(files[n].Data)
			n++
		}
		p := api.FilesParams{ID: box, Files: guestproto.Files{Files: files[:n]}}
		if n == len(files) {
			p.Watch = watch
		}
		err := rpc.Call(ctx, w.sess, api.BoxFiles, p, nil)
		if err != nil && strings.Contains(err.Error(), "unsupported request type") {
			// A box from before the user layer keeps what it has.
			return nil
		}
		if err != nil {
			return fmt.Errorf("write user layer: %w", err)
		}
		files = files[n:]
	}
	var script []string
	if l.GitName != "" {
		script = append(script, `git config --global user.name "$1"`)
	}
	if l.GitEmail != "" {
		script = append(script, `git config --global user.email "$2"`)
	}
	if hasGH {
		// Images without gh still get git over HTTPS, from the token in its login.
		script = append(script, `if command -v gh >/dev/null; then gh auth setup-git; else
git config --global credential.https://github.com.helper '!f() { echo username=x-access-token; echo "password=$(sed -n "s/^ *oauth_token: *//p" ~/.config/gh/hosts.yml | head -1)"; }; f'
fi`)
	}
	if len(script) == 0 {
		return nil
	}
	return s.run(ctx, w, box, api.ExecParams{Argv: []string{"sh", "-ec", strings.Join(script, "\n"), "sh", l.GitName, l.GitEmail}}, io.Discard, io.Discard)
}

// seedData encodes a seed for merging: JSON or TOML by the file's extension. A
// "{workspace}" key stands for each directory the box's threads may run in.
func seedData(seed agents.Seed, dirs []string) ([]byte, error) {
	merge := expandWorkspace(seed.Merge, dirs)
	switch {
	case strings.HasSuffix(seed.File, ".json"):
		return json.Marshal(merge)
	case strings.HasSuffix(seed.File, ".toml"):
		var b bytes.Buffer
		err := toml.NewEncoder(&b).Encode(merge)
		return b.Bytes(), err
	}
	return nil, errors.New("seeds must be .json or .toml files")
}

func expandWorkspace(m map[string]any, dirs []string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = expandWorkspace(sub, dirs)
		}
		if k != "{workspace}" {
			out[k] = v
			continue
		}
		for _, d := range dirs {
			out[d] = v
		}
	}
	return out
}

// installAgents runs every agent adapter's install command and the user's
// user-setup.sh in a template box, as dev.
func (s *Server) installAgents(ctx context.Context, w *workerConn, box string, stdout, stderr io.Writer) error {
	l, _, err := s.userLayer()
	if err != nil {
		return err
	}
	set, err := agents.Load(l.Adapters)
	if err != nil {
		return err
	}
	for _, a := range set.Sorted() {
		if a.Install == "" || !a.Agent() {
			continue
		}
		// An image that has the agent's command keeps its own.
		if s.hasCommand(ctx, w, box, a.Start) {
			fmt.Fprintf(stdout, "%s: the image has it\n", a.Name)
			continue
		}
		step(stdout, "install %s", a.Name)
		if err := s.run(ctx, w, box, api.ExecParams{Argv: []string{"bash", "-eo", "pipefail", "-c", a.Install}}, stdout, stderr); err != nil {
			return fmt.Errorf("install %s: %w", a.Name, err)
		}
	}
	if l.UserSetup != "" {
		step(stdout, "user-setup.sh (as dev)")
		if err := s.run(ctx, w, box, api.ExecParams{Argv: []string{"bash", "-eo", "pipefail", "-c", l.UserSetup}}, stdout, stderr); err != nil {
			return fmt.Errorf("user-setup.sh: %w", err)
		}
	}
	return nil
}

// userLoginBox starts a scratch box from the base image with the requested agents
// installed, for `hutch login` to run their logins in.
func (s *Server) userLoginBox(ctx context.Context, params json.RawMessage, out *rpc.Out) (any, error) {
	var p api.LoginParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	stdout, stderr := out.Writers()
	w, release, err := s.placeWait(ctx, need{memMiB: 4096, vcpus: 2}, "", "login box", stderr)
	if err != nil {
		return nil, err
	}
	defer release()
	suffix := make([]byte, 3)
	rand.Read(suffix)
	id := "login-" + hex.EncodeToString(suffix)
	step(stdout, "boot %s", id)
	b, err := s.callForBox(ctx, w, api.BoxBoot, api.BootParams{ID: id, Image: api.DefaultImage, VCPUs: 2, MemMiB: 4096,
		Idle: api.Idle{Timeout: time.Hour}})
	if err != nil {
		return nil, err
	}
	for _, name := range p.Adapters {
		a := set[name]
		if a == nil || a.Login == "" {
			s.removeBox(ctx, id)
			return nil, fmt.Errorf("no login for %q", name)
		}
		if a.Install != "" && !s.hasCommand(ctx, w, id, a.Start) {
			step(stdout, "install %s", name)
			if err := s.run(ctx, w, id, api.ExecParams{Argv: []string{"bash", "-eo", "pipefail", "-c", a.Install}}, stdout, stderr); err != nil {
				s.removeBox(ctx, id)
				return nil, err
			}
		}
	}
	return b, nil
}

// userCapture copies an adapter's credential files from a box into the user layer.
func (s *Server) userCapture(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.CaptureParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	set, err := s.adapters()
	if err != nil {
		return nil, err
	}
	a := set[p.Adapter]
	if a == nil {
		return nil, fmt.Errorf("no adapter %q", p.Adapter)
	}
	w, err := s.awake(ctx, p.Box)
	if err != nil {
		return nil, err
	}
	files, err := s.readSources(ctx, w, p.Box, a.Credentials)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s left no credentials in the box; did the login finish?", p.Adapter)
	}
	data, _ := json.Marshal(files)
	v, _, err := s.store.putLayerPart(credentialsPrefix+p.Adapter, -1, data)
	if err != nil {
		return nil, err
	}
	return api.Credentials{Part: p.Adapter, Version: v, Updated: time.Now().UTC()}, nil
}

// readSources reads adapter sources from a box; missing files are skipped.
func (s *Server) readSources(ctx context.Context, w *workerConn, box string, sources []agents.Source) ([]guestproto.File, error) {
	var files []guestproto.File
	for _, src := range sources {
		var data bytes.Buffer
		err := s.run(ctx, w, box, api.ExecParams{Argv: []string{"sh", "-c", `f="$HOME/${1#\~/}"; [ ! -f "$f" ] || cat -- "$f"`, "sh", src.File}}, &data, io.Discard)
		if err != nil {
			return nil, err
		}
		if data.Len() == 0 {
			continue
		}
		f := guestproto.File{Path: src.File, Mode: 0o600, Data: data.Bytes()}
		if len(src.Keys) > 0 {
			subset, err := agents.JSONKeys(data.Bytes(), src.Keys)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", src.File, err)
			}
			if subset == nil {
				continue
			}
			f.Data, f.Merge = subset, true
		}
		files = append(files, f)
	}
	return files, nil
}

// watchReport notices a box's credential files changing, which happens when an
// agent refreshes its login, and copies the new login to the user layer and every
// other running box: logins rotate refresh tokens, so the old copies stop working.
func (s *Server) watchReport(br api.BoxReport) {
	if len(br.Files) == 0 {
		return
	}
	id := br.Box.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	known := s.fileHashes[id]
	if known == nil || s.syncing[id] {
		if known == nil {
			s.fileHashes[id] = br.Files
		}
		return
	}
	var changed []string
	for p, h := range br.Files {
		if known[p] != h {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return
	}
	s.syncing[id] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.syncBack(ctx, id, changed); err != nil {
			log.Printf("credentials from %s: %v", id, err)
		}
		s.mu.Lock()
		s.fileHashes[id] = br.Files
		delete(s.syncing, id)
		s.mu.Unlock()
	}()
}

func (s *Server) syncBack(ctx context.Context, box string, changed []string) error {
	w, err := s.workerFor(box)
	if err != nil {
		return err
	}
	creds, err := s.store.boxCredentials(box)
	if err != nil {
		return err
	}
	set, err := s.adapters()
	if err != nil {
		return err
	}
	for _, a := range set.Sorted() {
		if !granted(creds, a.Name) || !slices.ContainsFunc(a.Credentials, func(src agents.Source) bool { return slices.Contains(changed, src.File) }) {
			continue
		}
		files, err := s.readSources(ctx, w, box, a.Credentials)
		if err != nil || len(files) == 0 {
			return err
		}
		part := credentialsPrefix + a.Name
		cur, err := s.store.layerPart(part)
		if err != nil {
			return err
		}
		data, _ := json.Marshal(files)
		if bytes.Equal(cur.content, data) {
			continue
		}
		if _, _, err := s.store.putLayerPart(part, -1, data); err != nil {
			return err
		}
		log.Printf("credentials: %s login refreshed in %s; updating the other running boxes", a.Name, box)
		s.pushCredentials(ctx, a.Name, box, files)
	}
	return nil
}

func (s *Server) pushCredentials(ctx context.Context, adapter, from string, files []guestproto.File) {
	boxes, err := s.store.boxes()
	if err != nil {
		log.Printf("credentials: %v", err)
		return
	}
	for _, b := range boxes {
		if b.ID == from || b.State != "running" {
			continue
		}
		creds, err := s.store.boxCredentials(b.ID)
		if err != nil || !granted(creds, adapter) {
			continue
		}
		w, err := s.worker(b.Worker)
		if err != nil {
			continue
		}
		if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: b.ID, Files: guestproto.Files{Files: files}}, nil); err != nil {
			log.Printf("credentials: push %s to %s: %v", adapter, b.ID, err)
		}
	}
}

// refreshCredentials writes the newest granted logins into a box that just woke:
// its snapshot may hold logins another box has since refreshed.
func (s *Server) refreshCredentials(ctx context.Context, w *workerConn, box string) {
	creds, err := s.store.boxCredentials(box)
	if err != nil || len(creds) == 0 {
		return
	}
	stored, err := s.credentials()
	if err != nil {
		return
	}
	var files []guestproto.File
	for _, c := range stored {
		if a, _, _ := strings.Cut(c.Part, "/"); granted(creds, a) {
			files = append(files, c.Files...)
		}
	}
	if len(files) == 0 {
		return
	}
	if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{Files: files}}, nil); err != nil {
		log.Printf("credentials: refresh %s after wake: %v", box, err)
	}
}

// hasCommand reports whether a box has the program a command line starts with.
func (s *Server) hasCommand(ctx context.Context, w *workerConn, box, command string) bool {
	f := strings.Fields(command)
	return len(f) > 0 && s.run(ctx, w, box, api.ExecParams{Argv: []string{"sh", "-c", "command -v " + f[0]}}, io.Discard, io.Discard) == nil
}

// agentTools are what agents and their installers expect in any box: downloads,
// process listing (Codex's background server uses ps), and git.
var agentTools = map[string]string{"curl": "curl", "ps": "procps", "git": "git"}

// ensureTools installs the agent tools an image lacks, with its own package manager.
func (s *Server) ensureTools(ctx context.Context, w *workerConn, box string, stdout, stderr io.Writer) error {
	var missing []string
	for cmd, pkg := range agentTools {
		if !s.hasCommand(ctx, w, box, cmd) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	pkgs := strings.Join(missing, " ")
	step(stdout, "install %s (agents need them; the image lacks them)", pkgs)
	script := `if command -v apt-get >/dev/null; then apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends ca-certificates $0 >/dev/null
elif command -v apk >/dev/null; then apk add -q ca-certificates bash $0
elif command -v dnf >/dev/null; then dnf install -yq ca-certificates $0
else echo "no package manager to install $0 with; add them to the image" >&2; exit 1; fi`
	return s.run(ctx, w, box, api.ExecParams{User: "root", Argv: []string{"sh", "-c", script, pkgs}}, stdout, stderr)
}
