package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

//go:embed prompts/setup.md
var setupPrompt string

// project resolves the repository in the current directory and its config.
func project(ctx context.Context, sess *yamux.Session) (*config.Repo, *config.Config, error) {
	repo, err := config.FindRepo(".")
	if err != nil {
		return nil, nil, err
	}
	if repo == nil {
		return nil, nil, errors.New("not in a Git repository")
	}
	cfg, err := loadConfig(ctx, sess)
	if err != nil {
		return nil, nil, err
	}
	return repo, cfg, nil
}

func git(root string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// templateParams describes a build of the recipe at HEAD, which must be committed
// and pushed: the VM clones it from the remote.
func templateParams(repo *config.Repo, cfg *config.Config, needCommit bool) (api.TemplateParams, error) {
	if strings.HasPrefix(repo.ID, "local:") {
		return api.TemplateParams{}, errors.New("the repository needs an origin remote: template VMs clone it from there")
	}
	p := api.TemplateParams{
		Project:      repo.ID,
		CloneURL:     "https://" + repo.ID + ".git",
		VCPUs:        int(cfg.Int("size.cpu")),
		MemMiB:       int(cfg.Size("size.memory") >> 20),
		DiskBytes:    cfg.Size("disk.size"),
		Idle:         api.Idle{Timeout: cfg.Duration("idle_timeout"), CPUThreshold: cfg.Percent("idle.cpu_threshold")},
		Credentials:  cfg.List("credentials"),
		PushInterval: cfg.Duration("git.push_interval"),
		Allow:        cfg.List("network.allow"),
		BuildTimeout: cfg.Duration("template.build_timeout"),
		QuietTimeout: cfg.Duration("template.quiet_timeout"),
	}
	if err := checkAllow(p.Allow); err != nil {
		return p, err
	}
	if !needCommit {
		return p, nil
	}
	// hutch.toml is read here, not in the VM, so the project form may have just changed it.
	if dirty, err := git(repo.Root, "status", "--porcelain", "--", ".hutch", ":!.hutch/hutch.toml"); err != nil {
		return p, err
	} else if dirty != "" {
		return p, fmt.Errorf("commit and push .hutch/ first; a template is built from a commit:\n%s", dirty)
	}
	commit, err := git(repo.Root, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	if remote, _ := git(repo.Root, "branch", "-r", "--contains", commit); remote == "" {
		return p, fmt.Errorf("HEAD (%.12s) is not on the remote; push it first", commit)
	}
	p.Commit = commit
	p.Image = cfg.String("image")
	if p.Recipe, err = recipeAt(repo.Root, commit); err != nil {
		return p, err
	}
	return p, nil
}

// maxRecipe caps .hutch/, which goes to the machine as the image's build context.
const maxRecipe = 8 << 20

// recipeAt reads .hutch/ at a commit, file name → content.
func recipeAt(root, commit string) (map[string][]byte, error) {
	list, err := git(root, "ls-tree", "-r", "--name-only", commit, "--", ".hutch")
	if err != nil {
		return nil, err
	}
	files, total := map[string][]byte{}, 0
	for _, path := range strings.Split(list, "\n") {
		if path == "" {
			continue
		}
		out, err := exec.Command("git", "-C", root, "show", commit+":"+path).Output()
		if err != nil {
			return nil, fmt.Errorf("git show %s: %w", path, err)
		}
		if total += len(out); total > maxRecipe {
			return nil, fmt.Errorf(".hutch/ is over %d MB; it's the image's build context, so keep large files out of it", maxRecipe>>20)
		}
		files[strings.TrimPrefix(path, ".hutch/")] = out
	}
	return files, nil
}

func initCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "init",
		Short:   "Set up this repository: a template box and a prompt for your agent",
		GroupID: "projects",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, cfg, err := project(ctx, sess)
			if err != nil {
				return err
			}
			p, err := templateParams(repo, cfg, false)
			if err != nil {
				return err
			}
			var proj api.Project
			if err := rpc.Call(ctx, sess, api.ProjectInit, api.ProjectParams{Project: repo.ID}, &proj); err != nil {
				return err
			}
			var b api.Box
			boxErr := rpc.CallOutput(ctx, sess, api.TemplateBox, p, &b, os.Stdout, os.Stderr)

			dir := filepath.Join(repo.Root, ".hutch")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if converted, err := convertSetup(dir); err != nil {
				return err
			} else if converted {
				fmt.Println("\nWrote .hutch/Dockerfile from .hutch/setup.sh, which templates no longer run directly.\n" +
					"  Steps that need the repository (its dependencies, a build) fail in the image:\n" +
					"  move them to .hutch/on-create.sh, which runs as dev in /workspace after checkout.\n" +
					"  Steps that enable systemd services: put the service's command in .hutch/services.")
			}
			var prompt bytes.Buffer
			err = template.Must(template.New("setup").Parse(setupPrompt)).Execute(&prompt, map[string]string{
				"Slug": proj.Slug, "TemplateBox": proj.Slug + "-template", "Node": "24", "DefaultRef": api.DefaultImageRef, "Arch": "x86_64 or arm64"})
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "SETUP_PROMPT.md"), prompt.Bytes(), 0o644); err != nil {
				return err
			}
			ignore := filepath.Join(dir, ".gitignore")
			if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(ignore, []byte("worktrees/\n"), 0o644); err != nil {
					return err
				}
			}

			fmt.Printf("\nProject %s (boxes named %s-…)\n", repo.ID, proj.Slug)
			if boxErr != nil {
				fmt.Fprintf(os.Stderr, "warning: the template box isn't ready: %v\n", boxErr)
			} else {
				fmt.Printf("Template box %s is running.\n", proj.Slug+"-template")
			}
			fmt.Printf("\nNext:\n  1. Give .hutch/SETUP_PROMPT.md to Claude Code or Codex in this repository.\n" +
				"  2. Review, commit, and push what it writes in .hutch/.\n  3. hutch template build\n")
			return nil
		}),
	}
}

// convertedDockerfile runs a project's old setup.sh as an image build step.
const convertedDockerfile = `FROM ` + api.DefaultImageRef + `
# Converted from setup.sh by hutch init. It runs without the repository: install
# the project's own dependencies in on-create.sh.
COPY setup.sh /tmp/hutch-setup.sh
RUN cd /tmp && DEBIAN_FRONTEND=noninteractive bash -eo pipefail /tmp/hutch-setup.sh && rm /tmp/hutch-setup.sh
`

// convertSetup writes a Dockerfile that runs setup.sh when a recipe has setup.sh
// and no Dockerfile.
func convertSetup(dir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, "setup.sh")); err != nil {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		return false, nil
	}
	return true, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(convertedDockerfile), 0o644)
}

func projectCommands() *cobra.Command {
	proj := &cobra.Command{Use: "project", Short: "Manage projects on the machine", GroupID: "projects"}
	var yes bool
	rm := &cobra.Command{
		Use:   "rm [<project>]",
		Short: "Remove a project's templates and secrets from the machine (default: this repository)",
		Args:  cobra.MaximumNArgs(1),
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else if repo, err := config.FindRepo("."); err != nil {
				return err
			} else if repo != nil {
				id = repo.ID
			}
			if id == "" {
				return errors.New("which project? hutch project rm <project>")
			}
			if !yes && !(term.IsTerminal(int(os.Stdin.Fd())) && confirm("Remove "+id+" and all its template versions from the machine?")) {
				return errors.New("not removed (pass --yes to skip the question)")
			}
			if err := rpc.Call(ctx, sess, api.ProjectRm, api.ProjectParams{Project: id}, nil); err != nil {
				return err
			}
			fmt.Println(id, "removed")
			return nil
		}),
	}
	rm.Flags().BoolVar(&yes, "yes", false, "don't ask")
	proj.AddCommand(rm)
	return proj
}

func templateCommands() *cobra.Command {
	tmpl := &cobra.Command{Use: "template", Short: "Build and manage this project's template", GroupID: "projects"}

	var buildFlags, snapFlags formFlags
	build := &cobra.Command{
		Use:   "build",
		Short: "Run the committed recipe in a fresh VM and verify it",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, cfg, err := project(ctx, sess)
			if err != nil {
				return err
			}
			if cfg, err = confirmSettings(ctx, sess, repo, cfg, buildFlags, false); err != nil {
				return err
			}
			p, err := templateParams(repo, cfg, true)
			if err != nil {
				return err
			}
			if err := syncSecrets(ctx, sess, repo); err != nil {
				return err
			}
			var b api.Box
			if err := rpc.CallOutput(ctx, sess, api.TemplateBuild, p, &b, os.Stdout, os.Stderr); err != nil {
				return err
			}
			fmt.Printf("\nLook around: hutch template shell\nFreeze it:   hutch template snapshot\n")
			return nil
		}),
	}

	snapshot := &cobra.Command{
		Use:   "snapshot",
		Short: "Freeze the recipe at HEAD as a new template version (builds it if needed)",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, cfg, err := project(ctx, sess)
			if err != nil {
				return err
			}
			if cfg, err = confirmSettings(ctx, sess, repo, cfg, snapFlags, false); err != nil {
				return err
			}
			p, err := templateParams(repo, cfg, true)
			if err != nil {
				return err
			}
			if err := syncSecrets(ctx, sess, repo); err != nil {
				return err
			}
			var t api.Template
			if err := rpc.CallOutput(ctx, sess, api.TemplateSnapshot, p, &t, os.Stdout, os.Stderr); err != nil {
				return err
			}
			fmt.Printf("New boxes in this project start from v%d: hutch new \"<prompt>\"\n", t.Version)
			return nil
		}),
	}

	shell := &cobra.Command{
		Use:   "shell",
		Short: "Open a shell on the template box",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, cfg, err := project(ctx, sess)
			if err != nil {
				return err
			}
			p, err := templateParams(repo, cfg, false)
			if err != nil {
				return err
			}
			var b api.Box
			if err := rpc.CallOutput(ctx, sess, api.TemplateBox, p, &b, os.Stderr, os.Stderr); err != nil {
				return err
			}
			return terminal(ctx, sess, b.ID, tmuxAttach("shell-1", "/workspace"))
		}),
	}

	versions := &cobra.Command{
		Use:   "versions",
		Short: "List template versions",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, err := config.FindRepo(".")
			if err != nil {
				return err
			}
			if repo == nil {
				return errors.New("not in a Git repository")
			}
			var ts []api.Template
			if err := rpc.Call(ctx, sess, api.TemplateList, api.ProjectParams{Project: repo.ID}, &ts); err != nil {
				return err
			}
			if len(ts) == 0 {
				fmt.Println("no template versions yet: hutch template snapshot")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tRECIPE\tFROM\tSIZE\tCREATED")
			for i, t := range ts {
				mark := ""
				if i == 0 {
					mark = "  (new boxes)"
				}
				fmt.Fprintf(tw, "v%d\t%.12s\t%s\t%d vCPU, %s\t%s%s\n", t.Version, t.Commit, shortImage(t.Image), t.VCPUs, mib(t.MemMiB), ago(t.Created), mark)
			}
			return tw.Flush()
		}),
	}

	buildFlags.register(build.Flags())
	snapFlags.register(snapshot.Flags())
	tmpl.AddCommand(build, snapshot, shell, versions)
	return tmpl
}

// templatePrompt tells an agent on the template box what its changes are for.
const templatePrompt = `You are in this project's template box. Hutch builds every box of the project from the recipe in .hutch/: an image from .hutch/Dockerfile (its build context is .hutch/), services from .hutch/services, then on-create.sh and verify.sh as dev in /workspace; on-create.sh also runs in each new box. Make this change in the recipe, try the commands here, and commit it on the current branch (hutch/template), which Hutch pushes. Anything changed outside the recipe is thrown away at the next build.

`

// templateThread starts a thread on the project's template box, starting the box if needed.
func templateThread(ctx context.Context, sess *yamux.Session, cfg *config.Config, agent agentFlags, args []string) error {
	repo, err := config.FindRepo(".")
	if err != nil {
		return err
	}
	if repo == nil {
		return errors.New("not in a Git repository")
	}
	p, err := templateParams(repo, cfg, false)
	if err != nil {
		return err
	}
	var b api.Box
	if err := rpc.CallOutput(ctx, sess, api.TemplateBox, p, &b, os.Stderr, os.Stderr); err != nil {
		return err
	}
	if len(args) > 0 && agent.cmd == "" {
		args = []string{templatePrompt + args[0]}
	}
	t, err := startThread(ctx, sess, cfg, b.ID, agent, args)
	if err != nil {
		return err
	}
	fmt.Printf("%s/%d %s started on the template box\n  hutch attach %s/%d\n", t.Box, t.N, t.Agent, t.Box, t.N)
	return nil
}

// syncSecrets uploads the files .hutch/uploads.txt lists when they differ from the
// project's stored copy. A file missing here is fine if the server has it: rebuilds
// don't need the machine that uploaded it.
func syncSecrets(ctx context.Context, sess *yamux.Session, repo *config.Repo) error {
	var stored api.Secrets
	if err := rpc.Call(ctx, sess, api.SecretsGet, api.ProjectParams{Project: repo.ID}, &stored); err != nil {
		return err
	}
	list, err := os.ReadFile(filepath.Join(repo.Root, ".hutch", "uploads.txt"))
	if errors.Is(err, os.ErrNotExist) {
		if stored.Digest == "" || len(stored.Files) == 0 {
			return nil
		}
		// No list means no secrets: don't put old ones into this build.
		fmt.Println("No .hutch/uploads.txt; removing the stored project secrets.")
		return rpc.Call(ctx, sess, api.SecretsPut, api.Secrets{Project: repo.ID}, nil)
	}
	if err != nil {
		return err
	}
	var files []guestproto.File
	var missing []string
	for _, line := range strings.Split(string(list), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		clean := filepath.ToSlash(filepath.Clean(p))
		if filepath.IsAbs(p) || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("uploads.txt: %q must be a path inside the repository", p)
		}
		data, err := os.ReadFile(filepath.Join(repo.Root, clean))
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, clean)
			continue
		}
		if err != nil {
			return err
		}
		files = append(files, guestproto.File{Path: clean, Data: data})
	}
	if len(missing) > 0 {
		if stored.Digest == "" {
			return fmt.Errorf("uploads.txt lists files that aren't here: %s", strings.Join(missing, ", "))
		}
		fmt.Fprintf(os.Stderr, "Not here: %s; using the machine's copy of the project secrets.\n", strings.Join(missing, ", "))
		return nil
	}
	if api.SecretsDigest(files) == stored.Digest {
		return nil
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	fmt.Printf("Uploading project secrets: %s\n", strings.Join(names, ", "))
	return rpc.Call(ctx, sess, api.SecretsPut, api.Secrets{Project: repo.ID, Files: files}, nil)
}

// checkAllow checks network.allow entries are host:port.
func checkAllow(allow []string) error {
	for _, a := range allow {
		host, port, err := net.SplitHostPort(a)
		if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("network.allow %q: want host:port", a)
		}
	}
	return nil
}

type boxSize struct{ cpu, memMiB int }

// Box sizes offered to projects.
var sizes = []boxSize{{2, 4096}, {4, 8192}, {8, 16384}, {16, 32768}}

func sizesCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "sizes",
		Short:   "Box size options and how many fit on your machine",
		GroupID: "projects",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			cfg, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			targets, err := fitTargets(ctx, sess)
			if err != nil {
				return err
			}
			margin := cfg.Percent("machine.memory_margin") / 100
			type option struct {
				CPU     int            `json:"cpu"`
				Memory  string         `json:"memory"`
				Current bool           `json:"current,omitempty"`
				Fits    map[string]int `json:"fits"` // boxes of this size per worker, by name
			}
			var opts []option
			project := boxSize{int(cfg.Int("size.cpu")), int(cfg.Size("size.memory") >> 20)}
			options := sizes
			if !slices.Contains(options, project) {
				options = append(slices.Clone(options), project)
			}
			for _, sz := range options {
				o := option{CPU: sz.cpu, Memory: mib(sz.memMiB), Fits: map[string]int{}, Current: sz == project}
				for _, t := range targets {
					o.Fits[t.name] = t.perWorker(sz, margin)
				}
				opts = append(opts, o)
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(opts)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SIZE\tFITS PER WORKER\t")
			for _, o := range opts {
				var fits []string
				for name, n := range o.Fits {
					fits = append(fits, fmt.Sprintf("%s: %d", name, n))
				}
				mark := ""
				if o.Current {
					mark = "(this project)"
				}
				fmt.Fprintf(tw, "%d vCPU, %s\t%s\t%s\n", o.CPU, o.Memory, strings.Join(fits, ", "), mark)
			}
			return tw.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// shortImage abbreviates the digests in "ref@sha256:…, ref@sha256:…".
func shortImage(image string) string {
	if image == "" {
		return "-"
	}
	parts := strings.Split(image, ", ")
	for i, p := range parts {
		if ref, digest, ok := strings.Cut(p, "@sha256:"); ok && len(digest) > 12 {
			parts[i] = ref + "@" + digest[:12]
		}
	}
	return strings.Join(parts, ", ")
}
