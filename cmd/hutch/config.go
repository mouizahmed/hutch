package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// loadConfig resolves every layer for the project in the current directory.
func loadConfig(ctx context.Context, sess *yamux.Session) (*config.Config, error) {
	var docs api.Configs
	if err := rpc.Call(ctx, sess, api.ConfigGet, struct{}{}, &docs); err != nil {
		return nil, err
	}
	srv, err := config.Parse(config.Server, "server config", []byte(docs.Server.Content))
	if err != nil {
		return nil, err
	}
	usr, err := config.ReadFile(config.User, config.UserPath())
	if err != nil {
		return nil, err
	}
	repo, err := config.FindRepo(".")
	if err != nil {
		return nil, err
	}
	c := config.New(srv, usr)
	if repo != nil {
		proj, err := config.ReadFile(config.Project, repo.ConfigPath())
		if err != nil {
			return nil, err
		}
		c = config.New(srv, usr, proj)
		c.ProjectID = repo.ID
	}
	return c, nil
}

// applyGrants asks about project requests beyond the user's grants. Declined or
// unattended, the box gets the granted subset and the user is told what was withheld.
func applyGrants(ctx context.Context, sess *yamux.Session, c *config.Config) (*config.Config, error) {
	withheld := c.Withheld()
	if len(withheld) == 0 {
		return c, nil
	}
	fmt.Fprintf(os.Stderr, "%s requests more than you have granted it:\n", c.ProjectID)
	for _, s := range withheld {
		fmt.Fprintf(os.Stderr, "  %s: %s (granted: %s)\n", s.Path, config.Display(s.Withheld), s)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !confirm("Grant this?") {
		fmt.Fprintln(os.Stderr, "Continuing with what is granted.")
		return c, nil
	}
	path := config.UserPath()
	text, err := readOptional(path)
	if err != nil {
		return nil, err
	}
	text, err = c.Grant(text, withheld)
	if err != nil {
		return nil, err
	}
	if err := writeUserConfig(text); err != nil {
		return nil, err
	}
	if err := syncUserConfig(ctx, sess, serverTarget(), ""); err != nil {
		return nil, err
	}
	return loadConfig(ctx, sess)
}

func confirm(q string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeUserConfig(text []byte) error {
	if _, err := config.Parse(config.User, config.UserPath(), text); err != nil {
		return err
	}
	return writeFileAtomic(config.UserPath(), text)
}

// The user config's last synced state, per server, so a sync can tell which side changed.
type syncState struct {
	Version int64  `json:"version"`
	SHA256  string `json:"sha256"`
}

func syncStatePath() string { return filepath.Join(filepath.Dir(config.UserPath()), ".synced.json") }

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// syncUserConfig reconciles the working copy with the server's authoritative copy:
// whichever side changed since the last sync wins; if both did, it refuses with a diff
// unless resolve is "mine" or "theirs".
func syncUserConfig(ctx context.Context, sess *yamux.Session, target, resolve string) error {
	path := config.UserPath()
	local, err := readOptional(path)
	if err != nil {
		return err
	}
	states := map[string]syncState{}
	if b, err := os.ReadFile(syncStatePath()); err == nil {
		json.Unmarshal(b, &states)
	}
	st, known := states[target]
	var docs api.Configs
	if err := rpc.Call(ctx, sess, api.ConfigGet, struct{}{}, &docs); err != nil {
		return err
	}
	srv := docs.User
	localChanged := sum(local) != st.SHA256
	if !known {
		localChanged = len(local) > 0
	}
	serverChanged := srv.Version != st.Version

	record := func(version int64, text []byte) error {
		states[target] = syncState{Version: version, SHA256: sum(text)}
		b, _ := json.MarshalIndent(states, "", "  ")
		return writeFileAtomic(syncStatePath(), b)
	}
	switch {
	case sum(local) == sum([]byte(srv.Content)) && resolve == "":
		if known && st.Version == srv.Version {
			return nil
		}
		return record(srv.Version, local)
	case resolve == "theirs" || (resolve == "" && !localChanged && serverChanged):
		if err := writeFileAtomic(path, []byte(srv.Content)); err != nil {
			return err
		}
		return record(srv.Version, []byte(srv.Content))
	case resolve == "mine" || (resolve == "" && localChanged && !serverChanged):
		if _, err := config.Parse(config.User, path, local); err != nil {
			return err
		}
		var res api.ConfigPutResult
		if err := rpc.Call(ctx, sess, api.ConfigPut, api.ConfigPutParams{Name: "user", BaseVersion: srv.Version, Content: string(local)}, &res); err != nil {
			return err
		}
		if res.Conflict {
			return errors.New("the server's copy of your config changed during the sync; run the command again")
		}
		return record(res.Doc.Version, local)
	case !localChanged && !serverChanged:
		return nil
	}
	return fmt.Errorf("your config changed here and on the server since this machine last synced:\n%s\n"+
		"keep this machine's: hutch config sync --mine\nkeep the server's:   hutch config sync --theirs",
		lineDiff(srv.Content, string(local)))
}

// lineDiff shows lines only on the server (-) or only here (+).
func lineDiff(server, local string) string {
	a, b := strings.Split(server, "\n"), strings.Split(local, "\n")
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			i, j = i+1, j+1
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, "  + "+b[j])
			j++
		default:
			out = append(out, "  - "+a[i])
			i++
		}
	}
	return "  (- server, + here)\n" + strings.Join(out, "\n")
}

type layerFlags struct{ project, user, server bool }

func (f *layerFlags) register(c *cobra.Command) {
	c.Flags().BoolVar(&f.project, "project", false, "the project's .hutch/hutch.toml")
	c.Flags().BoolVar(&f.user, "user", false, "your config (the default)")
	c.Flags().BoolVar(&f.server, "server", false, "the server's config")
	c.MarkFlagsMutuallyExclusive("project", "user", "server")
}

// layer picks the layer to edit: the flag, else user if the key allows it, else the
// key's only layer.
func (f *layerFlags) layer(k *config.Key) config.Layer {
	switch {
	case f.project:
		return config.Project
	case f.server:
		return config.Server
	case f.user || k == nil:
		return config.User
	}
	for _, l := range k.Layers {
		if l == config.User {
			return l
		}
	}
	return k.Layers[0]
}

// editLayer reads a layer's text, applies change, checks the result, and saves it.
func editLayer(ctx context.Context, sess *yamux.Session, layer config.Layer, change func([]byte) ([]byte, error)) error {
	switch layer {
	case config.Server:
		var docs api.Configs
		if err := rpc.Call(ctx, sess, api.ConfigGet, struct{}{}, &docs); err != nil {
			return err
		}
		text, err := change([]byte(docs.Server.Content))
		if err != nil {
			return err
		}
		var res api.ConfigPutResult
		if err := rpc.Call(ctx, sess, api.ConfigPut, api.ConfigPutParams{Name: "server", BaseVersion: docs.Server.Version, Content: string(text)}, &res); err != nil {
			return err
		}
		if res.Conflict {
			return errors.New("the server config changed meanwhile; try again")
		}
		return nil
	case config.User:
		text, err := readOptional(config.UserPath())
		if err != nil {
			return err
		}
		if text, err = change(text); err != nil {
			return err
		}
		if err := writeUserConfig(text); err != nil {
			return err
		}
		return syncUserConfig(ctx, sess, serverTarget(), "")
	default:
		repo, err := config.FindRepo(".")
		if err != nil {
			return err
		}
		if repo == nil {
			return errors.New("not in a Git repository")
		}
		path := repo.ConfigPath()
		text, err := readOptional(path)
		if err != nil {
			return err
		}
		if text, err = change(text); err != nil {
			return err
		}
		if _, err := config.Parse(config.Project, path, text); err != nil {
			return err
		}
		return writeFileAtomic(path, text)
	}
}

func withVersion(text []byte) []byte {
	if len(strings.TrimSpace(string(text))) == 0 {
		return []byte(fmt.Sprintf("version = %d\n", config.Version))
	}
	return text
}

// editInEditor opens text in $VISUAL or $EDITOR until it parses or the user gives up.
func editInEditor(text []byte, layer config.Layer, name string) ([]byte, error) {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "hutch-*.toml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	f.Write(withVersion(text))
	f.Close()
	for {
		cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", f.Name())
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%s: %w", editor, err)
		}
		out, err := os.ReadFile(f.Name())
		if err != nil {
			return nil, err
		}
		_, perr := config.Parse(layer, name, out)
		if perr == nil {
			return out, nil
		}
		fmt.Fprintln(os.Stderr, perr)
		if !confirm("Edit again?") {
			return nil, errors.New("not saved")
		}
	}
}

func configCommands() *cobra.Command {
	cfgCmd := &cobra.Command{Use: "config", Short: "Show and change settings", GroupID: "setup"}

	get := &cobra.Command{
		Use:   "get <key>",
		Short: "Print a setting's effective value and where it comes from",
		Args:  cobra.ExactArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			c, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			s, err := c.Get(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s\t(%s)\n", s, from(s))
			if s.Withheld != nil {
				fmt.Printf("withheld: %s (not granted to %s)\n", config.Display(s.Withheld), c.ProjectID)
			}
			return nil
		}),
	}

	var setLayer layerFlags
	setOnServer := withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			path, err := config.SplitPath(args[0])
			if err != nil {
				return err
			}
			k := config.Lookup(path)
			if k == nil {
				return fmt.Errorf("unknown key %s", args[0])
			}
			raw, err := k.FromString(args[1])
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if _, err := k.Parse(raw); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			layer := setLayer.layer(k)
			return editLayer(ctx, sess, layer, func(text []byte) ([]byte, error) {
				return config.Set(withVersion(text), path, raw)
			})
		})
	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Change a setting in one layer (lists are comma-separated)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The server key says where the server is, so changing it can't go
			// through the old one, which may be gone.
			if args[0] == "server" {
				if err := setUserServer(args[1]); err != nil {
					return err
				}
				// This machine's config, with the new address, is the one to keep.
				sess, err := dial()
				if err != nil {
					fmt.Printf("server = %q; it isn't reachable yet (%v)\n", args[1], err)
					return nil
				}
				defer sess.Close()
				return syncUserConfig(cmd.Context(), sess, serverTarget(), "mine")
			}
			return setOnServer(cmd, args)
		},
	}
	setLayer.register(set)

	var editFlags layerFlags
	var projectFlags formFlags
	edit := &cobra.Command{
		Use:   "edit",
		Short: "Edit one layer in your editor (--project opens the project form)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			layer := editFlags.layer(nil)
			if layer == config.Project {
				sess, err := connect()
				if err != nil {
					return err
				}
				defer sess.Close()
				repo, cfg, err := project(cmd.Context(), sess)
				if err != nil {
					return err
				}
				_, err = confirmSettings(cmd.Context(), sess, repo, cfg, projectFlags, true)
				return err
			}
			change := func(text []byte) ([]byte, error) { return editInEditor(text, layer, layer.String()+" config") }
			// The user config is edited before connecting, so a broken one can be fixed.
			if layer == config.User {
				text, err := readOptional(config.UserPath())
				if err != nil {
					return err
				}
				if text, err = change(text); err != nil {
					return err
				}
				if err := writeUserConfig(text); err != nil {
					return err
				}
			}
			sess, err := connect()
			if err != nil {
				return err
			}
			defer sess.Close()
			if layer == config.User {
				return nil
			}
			return editLayer(cmd.Context(), sess, layer, change)
		},
	}
	editFlags.register(edit)
	projectFlags.register(edit.Flags())

	explain := &cobra.Command{
		Use:   "explain",
		Short: "Print the effective config for this project, with sources",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			c, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			if c.ProjectID != "" {
				fmt.Printf("project %s\n\n", c.ProjectID)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tVALUE\tFROM")
			for _, s := range c.All() {
				note := ""
				if s.Withheld != nil {
					note = "  withheld: " + config.Display(s.Withheld)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s%s\n", s.Path, s, from(s), note)
			}
			return tw.Flush()
		}),
	}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check every layer against the schema",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			c, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			for l := config.Server; l <= config.Project; l++ {
				if d := c.Doc(l); d != nil {
					fmt.Printf("%-8s ok  %s\n", l, d.Source)
				}
			}
			return nil
		}),
	}

	var revoke bool
	grants := &cobra.Command{
		Use:   "grants [<project>]",
		Short: "Show what you've granted projects, or revoke it",
		Args:  cobra.MaximumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			if revoke {
				if len(args) == 0 {
					return errors.New("which project? hutch config grants <project> --revoke")
				}
				return editLayer(ctx, sess, config.User, func(text []byte) ([]byte, error) {
					return config.Remove(text, []string{"grants", args[0]})
				})
			}
			c, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			byProject := map[string][]string{}
			for _, s := range c.All() {
				p, _ := config.SplitPath(s.Path)
				if len(p) > 2 && p[0] == "grants" && (len(args) == 0 || p[1] == args[0]) {
					byProject[p[1]] = append(byProject[p[1]], fmt.Sprintf("  %s = %s", config.FormatPath(p[2:]), s))
				}
			}
			if len(byProject) == 0 {
				fmt.Println("no grants")
			}
			projects := make([]string, 0, len(byProject))
			for p := range byProject {
				projects = append(projects, p)
			}
			sort.Strings(projects)
			for _, p := range projects {
				fmt.Println(p)
				fmt.Println(strings.Join(byProject[p], "\n"))
			}
			return nil
		}),
	}
	grants.Flags().BoolVar(&revoke, "revoke", false, "remove everything granted to the project")

	var mine, theirs bool
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync your config with the server's copy (runs before every command)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolve := ""
			switch {
			case mine:
				resolve = "mine"
			case theirs:
				resolve = "theirs"
			}
			sess, err := dial()
			if err != nil {
				return err
			}
			defer sess.Close()
			return syncUserConfig(cmd.Context(), sess, serverTarget(), resolve)
		},
	}
	syncCmd.Flags().BoolVar(&mine, "mine", false, "replace the server's copy with this machine's")
	syncCmd.Flags().BoolVar(&theirs, "theirs", false, "replace this machine's copy with the server's")
	syncCmd.MarkFlagsMutuallyExclusive("mine", "theirs")

	cfgCmd.AddCommand(get, set, edit, explain, validate, grants, syncCmd)
	return cfgCmd
}

func from(s config.Setting) string {
	if s.Layer == config.Default || s.Layer == config.Flag {
		return s.Layer.String()
	}
	return s.Layer.String() + ": " + s.Source
}
