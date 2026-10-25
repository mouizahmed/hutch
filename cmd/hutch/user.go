package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
	"github.com/mouizahmed/hutch/internal/wire"
)

// maxConfigBytes bounds what `hutch user sync` uploads, so a stray cache directory
// in an agent's config doesn't end up in every box.
const maxConfigBytes = 50 << 20

const boxHome = "/home/dev"

// collected is what `hutch user sync` reads from this machine.
type collected struct {
	layer    api.UserLayer
	secrets  map[string][]guestproto.File
	warnings []string
}

// notInBoxes matches paths that won't exist in a Linux box.
var notInBoxes = regexp.MustCompile(`/Users/[^/"'\s]+|/opt/homebrew[^"'\s]*|/Applications/[^"'\s]+|C:\\\\[^"'\s]+`)

func collectLayer() (collected, error) {
	c := collected{layer: api.UserLayer{Adapters: map[string]string{}, Files: map[string][]guestproto.File{}},
		secrets: map[string][]guestproto.File{}}
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	dir := filepath.Join(filepath.Dir(config.UserPath()), "agents")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".toml" {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return c, err
			}
			c.layer.Adapters[e.Name()] = string(data)
		}
	}
	set, err := agents.Load(c.layer.Adapters)
	if err != nil {
		return c, err
	}
	total := 0
	for _, a := range set.Sorted() {
		for _, src := range a.Config {
			files, err := readLocal(home, src)
			if err != nil {
				return c, err
			}
			for _, f := range files {
				if a.MCP != nil && a.MCP.File == src.File {
					var secret *guestproto.File
					if f, secret, err = splitSecrets(f, a.MCP.Key); err != nil {
						return c, fmt.Errorf("%s: %w", src.File, err)
					}
					if secret != nil {
						c.secrets[a.Name] = append(c.secrets[a.Name], *secret)
					}
				}
				f.Data = bytes.ReplaceAll(f.Data, []byte(home+"/"), []byte(boxHome+"/"))
				for _, m := range notInBoxes.FindAll(f.Data, 3) {
					c.warnings = append(c.warnings, fmt.Sprintf("%s mentions %s, which won't exist in a box", f.Path, m))
				}
				total += len(f.Data)
				c.layer.Files[a.Name] = append(c.layer.Files[a.Name], f)
			}
		}
	}
	if total > maxConfigBytes {
		return c, fmt.Errorf("agent config adds up to %d MB, over the %d MB limit; check the directories adapters list under config", total>>20, maxConfigBytes>>20)
	}
	c.layer.GitName = gitGlobal("user.name")
	c.layer.GitEmail = gitGlobal("user.email")
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(config.UserPath()), "user-setup.sh")); err == nil {
		c.layer.UserSetup = string(b)
	}
	return c, nil
}

func gitGlobal(key string) string {
	out, _ := exec.Command("git", "config", "--global", "--get", key).Output()
	return strings.TrimSpace(string(out))
}

// readLocal reads one adapter source from this machine; missing paths are skipped.
func readLocal(home string, src agents.Source) ([]guestproto.File, error) {
	path := filepath.Join(home, strings.TrimPrefix(src.File, "~/"))
	if src.Dir() {
		var files []guestproto.File
		err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, _ := d.Info()
			rel, _ := filepath.Rel(home, p)
			files = append(files, guestproto.File{Path: "~/" + filepath.ToSlash(rel), Mode: uint32(info.Mode().Perm()), Data: data})
			return nil
		})
		return files, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := guestproto.File{Path: src.File, Mode: 0o600, Data: data}
	if len(src.Keys) > 0 {
		subset, err := agents.JSONKeys(data, src.Keys)
		if err != nil || subset == nil {
			return nil, err
		}
		f.Data, f.Merge = subset, true
	}
	return []guestproto.File{f}, nil
}

// splitSecrets moves the env blocks of MCP servers under key out of a config file,
// into a separate file merged into the same path from the credentials part.
func splitSecrets(f guestproto.File, key string) (guestproto.File, *guestproto.File, error) {
	var doc map[string]any
	toml_ := strings.HasSuffix(f.Path, ".toml")
	var err error
	if toml_ {
		_, err = toml.Decode(string(f.Data), &doc)
	} else {
		err = json.Unmarshal(f.Data, &doc)
	}
	if err != nil {
		return f, nil, err
	}
	servers, _ := doc[key].(map[string]any)
	envs := map[string]any{}
	for name, s := range servers {
		if sm, ok := s.(map[string]any); ok && sm["env"] != nil {
			envs[name] = map[string]any{"env": sm["env"]}
			delete(sm, "env")
		}
	}
	if len(envs) == 0 {
		return f, nil, nil
	}
	encode := func(v any) ([]byte, error) {
		if toml_ {
			var b bytes.Buffer
			err := toml.NewEncoder(&b).Encode(v)
			return b.Bytes(), err
		}
		return json.MarshalIndent(v, "", "  ")
	}
	if f.Data, err = encode(doc); err != nil {
		return f, nil, err
	}
	data, err := encode(map[string]any{key: envs})
	if err != nil {
		return f, nil, err
	}
	return f, &guestproto.File{Path: f.Path, Mode: 0o600, Data: data, Merge: true}, nil
}

func fileKey(adapter string, f guestproto.File) string { return adapter + " " + f.Path }

func sumBytes(b []byte) [32]byte { return sha256.Sum256(b) }

// layerDiff describes how next differs from cur, one line per change.
func layerDiff(cur, next api.UserLayer) []string {
	var lines []string
	old := map[string][32]byte{}
	for a, files := range cur.Files {
		for _, f := range files {
			old[fileKey(a, f)] = sumBytes(f.Data)
		}
	}
	seen := map[string]bool{}
	for a, files := range next.Files {
		for _, f := range files {
			k := fileKey(a, f)
			seen[k] = true
			if prev, ok := old[k]; !ok {
				lines = append(lines, "+ "+f.Path)
			} else if prev != sumBytes(f.Data) {
				lines = append(lines, "~ "+f.Path)
			}
		}
	}
	for k := range old {
		if !seen[k] {
			_, path, _ := strings.Cut(k, " ")
			lines = append(lines, "- "+path)
		}
	}
	for name, data := range next.Adapters {
		if prev, ok := cur.Adapters[name]; !ok {
			lines = append(lines, "+ adapter "+name)
		} else if prev != data {
			lines = append(lines, "~ adapter "+name)
		}
	}
	for name := range cur.Adapters {
		if _, ok := next.Adapters[name]; !ok {
			lines = append(lines, "- adapter "+name)
		}
	}
	if cur.GitName != next.GitName || cur.GitEmail != next.GitEmail {
		lines = append(lines, fmt.Sprintf("~ git identity: %s <%s>", next.GitName, next.GitEmail))
	}
	if cur.UserSetup != next.UserSetup {
		lines = append(lines, "~ user-setup.sh (baked in at the next template build)")
	}
	sort.Strings(lines)
	return lines
}

func userCommands() *cobra.Command {
	userCmd := &cobra.Command{Use: "user", Short: "Your agent config, MCP servers, and logins for every box", GroupID: "setup"}

	var yes bool
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Copy agent config and MCP servers from this machine into the user layer",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			c, err := collectLayer()
			if err != nil {
				return err
			}
			var st api.UserState
			if err := rpc.Call(ctx, sess, api.UserGet, struct{}{}, &st); err != nil {
				return err
			}
			for _, w := range c.warnings {
				fmt.Fprintln(os.Stderr, "warning:", w)
			}
			diff := layerDiff(st.Layer, c.layer)
			if len(diff) == 0 && st.Version > 0 {
				fmt.Println("The user layer is up to date.")
				return nil
			}
			fmt.Println("Changes to the user layer:")
			for _, l := range diff {
				fmt.Println("  " + l)
			}
			for a, s := range c.secrets {
				fmt.Printf("  MCP secrets for %s kept apart from config (%d file)\n", a, len(s))
			}
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return errors.New("not a terminal; pass --yes to apply")
				}
				if !confirm("Apply?") {
					return errors.New("not applied")
				}
			}
			p := api.UserPutConfigParams{BaseVersion: st.Version, Layer: c.layer, Secrets: c.secrets}
			if err := rpc.Call(ctx, sess, api.UserPutConfig, p, nil); err != nil {
				return err
			}
			fmt.Println("Applied. New boxes get it; running boxes keep what they have.")
			return nil
		}),
	}
	syncCmd.Flags().BoolVar(&yes, "yes", false, "apply without asking")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show what the user layer holds",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			var st api.UserState
			if err := rpc.Call(ctx, sess, api.UserGet, struct{}{}, &st); err != nil {
				return err
			}
			set, err := agents.Load(st.Layer.Adapters)
			if err != nil {
				return err
			}
			logins := map[string]api.Credentials{}
			for _, c := range st.Credentials {
				logins[c.Part] = c
			}
			for _, a := range set.Sorted() {
				line := fmt.Sprintf("%-8s config: %d files", a.Name, len(st.Layer.Files[a.Name]))
				if a.Login != "" {
					if c, ok := logins[a.Name]; ok {
						line += fmt.Sprintf(", logged in (updated %s)", ago(c.Updated))
					} else {
						line += ", not logged in: hutch login " + a.Name
					}
				}
				if _, ok := logins[a.Name+"/mcp"]; ok {
					line += ", MCP secrets"
				}
				fmt.Println(line)
			}
			if st.Layer.GitName != "" || st.Layer.GitEmail != "" {
				fmt.Printf("git      %s <%s>\n", st.Layer.GitName, st.Layer.GitEmail)
			}
			if st.Layer.UserSetup != "" {
				fmt.Println("user-setup.sh present")
			}
			if st.Version == 0 {
				fmt.Println("\nNothing synced yet: hutch user sync")
			}
			return nil
		}),
	}

	userCmd.AddCommand(syncCmd, status)
	return userCmd
}

func loginCommand() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:     "login [<agent>...]",
		Short:   "Log in to agents and GitHub inside a box; every box gets the logins",
		GroupID: "setup",
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			var st api.UserState
			if err := rpc.Call(ctx, sess, api.UserGet, struct{}{}, &st); err != nil {
				return err
			}
			set, err := agents.Load(st.Layer.Adapters)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				for _, a := range set.Sorted() {
					if a.Login != "" {
						names = append(names, a.Name)
					}
				}
			}
			for _, n := range names {
				if a := set[n]; a == nil || a.Login == "" {
					return fmt.Errorf("no login for %q", n)
				}
			}
			if from != "" {
				for _, n := range names {
					var c api.Credentials
					if err := rpc.Call(ctx, sess, api.UserCapture, api.CaptureParams{Box: from, Adapter: n}, &c); err != nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", n, err)
						continue
					}
					fmt.Printf("Saved the %s login from %s.\n", n, from)
				}
				return nil
			}
			var b api.Box
			if err := rpc.CallOutput(ctx, sess, api.UserLoginBox, api.LoginParams{Adapters: names}, &b, os.Stdout, os.Stderr); err != nil {
				return err
			}
			defer rpc.Call(context.Background(), sess, api.BoxRm, api.IDParams{ID: b.ID}, nil)
			for _, n := range names {
				fmt.Printf("\n==> %s: %s\n", n, set[n].Login)
				stop := forwardNewPorts(ctx, sess, b.ID)
				err := terminal(ctx, sess, b.ID, []string{"bash", "-lc", set[n].Login})
				stop()
				if err != nil {
					return fmt.Errorf("%s login: %w", n, err)
				}
				var c api.Credentials
				if err := rpc.Call(ctx, sess, api.UserCapture, api.CaptureParams{Box: b.ID, Adapter: n}, &c); err != nil {
					return err
				}
				fmt.Printf("Saved the %s login; new boxes get it.\n", n)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&from, "from", "", "save logins already made in this box instead of logging in again")
	return cmd
}

// forwardNewPorts makes ports that start listening in the box reachable on this
// machine's localhost, so a login's browser callback reaches the box.
func forwardNewPorts(ctx context.Context, sess *yamux.Session, box string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	ports := func() map[int]bool {
		var st guestproto.Status
		if err := rpc.Call(ctx, sess, api.BoxStatus, api.IDParams{ID: box}, &st); err != nil {
			return nil
		}
		m := map[int]bool{}
		for _, p := range st.Ports {
			m[p.Port] = true
		}
		return m
	}
	before := ports()
	var mu sync.Mutex
	var listeners []net.Listener
	go func() {
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
			for port := range ports() {
				if before[port] {
					continue
				}
				before[port] = true
				ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					fmt.Fprintf(os.Stderr, "\r\n[hutch] can't forward localhost:%d: %v\r\n", port, err)
					continue
				}
				mu.Lock()
				listeners = append(listeners, ln)
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "\r\n[hutch] localhost:%d now reaches the box\r\n", port)
				go serveForward(ctx, sess, box, port, ln)
			}
		}
	}()
	return func() {
		cancel()
		mu.Lock()
		defer mu.Unlock()
		for _, ln := range listeners {
			ln.Close()
		}
	}
}

func serveForward(ctx context.Context, sess *yamux.Session, box string, port int, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			conn, err := rpc.OpenStream(ctx, sess, api.BoxDial, api.DialParams{ID: box, Port: port})
			if err != nil {
				return
			}
			var r guestproto.Response
			if err := wire.Read(conn, &r); err != nil || r.Type != guestproto.RespOK {
				conn.Close()
				return
			}
			rpc.Splice(c, conn)
		}()
	}
}
