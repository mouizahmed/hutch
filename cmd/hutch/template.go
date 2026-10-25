package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/hashicorp/yamux"
	"github.com/spf13/cobra"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
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
		BuildTimeout: cfg.Duration("template.build_timeout"),
		QuietTimeout: cfg.Duration("template.quiet_timeout"),
	}
	if !needCommit {
		return p, nil
	}
	if dirty, err := git(repo.Root, "status", "--porcelain", "--", ".hutch"); err != nil {
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
	return p, nil
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
			var prompt bytes.Buffer
			err = template.Must(template.New("setup").Parse(setupPrompt)).Execute(&prompt, map[string]string{
				"Slug": proj.Slug, "TemplateBox": proj.Slug + "-template", "Node": "24"})
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

func templateCommands() *cobra.Command {
	tmpl := &cobra.Command{Use: "template", Short: "Build and manage this project's template", GroupID: "projects"}

	build := &cobra.Command{
		Use:   "build",
		Short: "Run the committed recipe in a fresh VM and verify it",
		Args:  cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			repo, cfg, err := project(ctx, sess)
			if err != nil {
				return err
			}
			p, err := templateParams(repo, cfg, true)
			if err != nil {
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
			p, err := templateParams(repo, cfg, true)
			if err != nil {
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
			fmt.Fprintln(tw, "VERSION\tRECIPE\tSIZE\tCREATED")
			for i, t := range ts {
				mark := ""
				if i == 0 {
					mark = "  (new boxes)"
				}
				fmt.Fprintf(tw, "v%d\t%.12s\t%d vCPU, %s\t%s%s\n", t.Version, t.Commit, t.VCPUs, mib(t.MemMiB), ago(t.Created), mark)
			}
			return tw.Flush()
		}),
	}

	tmpl.AddCommand(build, snapshot, shell, versions)
	return tmpl
}

// templatePrompt tells an agent on the template box what its changes are for.
const templatePrompt = `You are in this project's template box. Hutch builds every box of the project from the recipe in .hutch/: setup.sh runs as root in a fresh VM, verify.sh must pass, and on-create.sh runs in each new box. Make this change in the recipe, try it here, and commit it on the current branch (hutch/template), which Hutch pushes. Anything changed outside the recipe is thrown away at the next build.

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

type boxSize struct{ cpu, memMiB int }

// Box sizes offered to projects.
var sizes = []boxSize{{2, 4096}, {4, 8192}, {8, 16384}, {16, 32768}}

func sizesCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "sizes",
		Short:   "Box size options and how many fit on your workers",
		GroupID: "projects",
		Args:    cobra.NoArgs,
		RunE: withServer(func(ctx context.Context, sess *yamux.Session, _ []string) error {
			cfg, err := loadConfig(ctx, sess)
			if err != nil {
				return err
			}
			var ws []api.Worker
			if err := rpc.Call(ctx, sess, api.WorkerList, struct{}{}, &ws); err != nil {
				return err
			}
			margin := cfg.Percent("worker.memory_margin") / 100
			type option struct {
				CPU     int            `json:"cpu"`
				Memory  string         `json:"memory"`
				Current bool           `json:"current,omitempty"`
				Fits    map[string]int `json:"fits"` // boxes of this size per online worker, by name
			}
			var opts []option
			project := boxSize{int(cfg.Int("size.cpu")), int(cfg.Size("size.memory") >> 20)}
			options := sizes
			if !slices.Contains(options, project) {
				options = append(slices.Clone(options), project)
			}
			for _, sz := range options {
				o := option{CPU: sz.cpu, Memory: mib(sz.memMiB), Fits: map[string]int{}, Current: sz == project}
				for _, w := range ws {
					if w.Online && w.MemMiB > 0 {
						o.Fits[w.Name] = int(float64(w.MemMiB) * (1 - margin) / float64(sz.memMiB))
					}
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
