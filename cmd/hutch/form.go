package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/hashicorp/yamux"
	"golang.org/x/term"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The keys the project form sets.
var formKeys = []string{"size.cpu", "size.memory", "disk.size", "default_agent", "idle_timeout", "network.allow"}

// settingsDigest identifies the effective project settings the form covers.
func settingsDigest(cfg *config.Config) string {
	h := sha256.New()
	for _, k := range formKeys {
		s, _ := cfg.Get(k)
		fmt.Fprintf(h, "%s=%s\n", k, s)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// formFlags set project settings without the form.
type formFlags struct {
	cpu        int
	memory     string
	disk, idle string
	yes        bool
}

func (f *formFlags) register(flags interface {
	IntVar(*int, string, int, string)
	StringVar(*string, string, string, string)
	BoolVar(*bool, string, bool, string)
}) {
	flags.IntVar(&f.cpu, "cpu", 0, "vCPUs of every box (skips the project form)")
	flags.StringVar(&f.memory, "memory", "", "memory of every box, like 8G (skips the project form)")
	flags.StringVar(&f.disk, "disk", "", "disk of every box, like 50G (skips the project form)")
	flags.StringVar(&f.idle, "idle", "", "idle timeout, like 10m (skips the project form)")
	flags.BoolVar(&f.yes, "yes", false, "accept the project settings as they are")
}

func (f *formFlags) set() map[string]any {
	m := map[string]any{}
	if f.cpu > 0 {
		m["size.cpu"] = int64(f.cpu)
	}
	if f.memory != "" {
		m["size.memory"] = f.memory
	}
	if f.disk != "" {
		m["disk.size"] = f.disk
	}
	if f.idle != "" {
		m["idle_timeout"] = f.idle
	}
	return m
}

// confirmSettings makes sure the project settings were confirmed: flags are written
// as given; otherwise the project form opens when the settings changed since they
// were last confirmed (or always, with force). It returns the config to build with.
func confirmSettings(ctx context.Context, sess *yamux.Session, repo *config.Repo, cfg *config.Config, flags formFlags, force bool) (*config.Config, error) {
	var proj api.Project
	if err := rpc.Call(ctx, sess, api.ProjectInit, api.ProjectParams{Project: repo.ID}, &proj); err != nil {
		return nil, err
	}
	path := repo.ConfigPath()
	text, err := readOptional(path)
	if err != nil {
		return nil, err
	}
	changes := flags.set()
	switch {
	case len(changes) > 0, flags.yes:
	case !term.IsTerminal(int(os.Stdin.Fd())):
		if force {
			return nil, fmt.Errorf("the project form needs a terminal; set values with --cpu, --memory, --disk, --idle")
		}
	case force || settingsDigest(cfg) != proj.Confirmed:
		if changes, err = projectForm(ctx, sess, cfg, text, proj); err != nil {
			return nil, err
		}
	}
	if len(changes) > 0 {
		text = withVersion(text)
		for _, k := range formKeys {
			raw, ok := changes[k]
			if !ok {
				continue
			}
			p, _ := config.SplitPath(k)
			if _, err := config.Lookup(p).Parse(raw); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			if text, err = config.Set(text, p, raw); err != nil {
				return nil, err
			}
		}
		if _, err := config.Parse(config.Project, path, text); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(path, text); err != nil {
			return nil, err
		}
		fmt.Printf("Wrote %s; commit it so every machine builds with these settings.\n", path)
		if cfg, err = loadConfig(ctx, sess); err != nil {
			return nil, err
		}
	}
	return cfg, rpc.Call(ctx, sess, api.ProjectConfirm, api.ConfirmParams{Project: repo.ID, Settings: settingsDigest(cfg)}, nil)
}

// projectForm asks for the project settings, preselecting the current values (the
// agent's proposals) with its reasons, and returns the keys that changed.
func projectForm(ctx context.Context, sess *yamux.Session, cfg *config.Config, text []byte, proj api.Project) (map[string]any, error) {
	targets, err := fitTargets(ctx, sess)
	if err != nil {
		return nil, err
	}
	margin := cfg.Percent("machine.memory_margin") / 100
	reason := func(key string) string {
		p, _ := config.SplitPath(key)
		if c := config.Comment(text, p); c != "" {
			return "Your agent's reason: " + c
		}
		return ""
	}

	cur := boxSize{int(cfg.Int("size.cpu")), int(cfg.Size("size.memory") >> 20)}
	sizeOpts := sizes
	if !slices.Contains(sizeOpts, cur) {
		sizeOpts = append(slices.Clone(sizeOpts), cur)
	}
	var sizeChoices []huh.Option[string]
	unfit := map[string]bool{}
	for _, sz := range sizeOpts {
		var fits []string
		for _, t := range targets {
			if n := t.perWorker(sz, margin); n > 0 {
				fits = append(fits, fmt.Sprintf("%d on %s", n, t.name))
			}
		}
		label := fmt.Sprintf("%d vCPU, %s", sz.cpu, mib(sz.memMiB))
		value := fmt.Sprintf("%d/%d", sz.cpu, sz.memMiB)
		switch {
		case len(fits) > 0:
			label += "   fits " + strings.Join(fits, ", ")
		case len(targets) > 0:
			label += "   doesn't fit the machine"
			unfit[value] = true
		}
		if sz == cur {
			label += "   (current)"
		}
		sizeChoices = append(sizeChoices, huh.NewOption(label, value))
	}
	sizeDesc := []string{"Every box in this project gets this size; changing it later rebuilds the template snapshot."}
	if r := reason("size.cpu"); r != "" {
		sizeDesc = append(sizeDesc, r)
	}
	if pk := proj.Peaks; pk != nil {
		sizeDesc = append(sizeDesc, fmt.Sprintf("The last build peaked at %.1f GB and %.1f vCPUs (in %d vCPU, %s).",
			float64(pk.MemMiB)/1024, pk.CPUs, pk.OfVCPUs, mib(pk.OfMiB)))
	}
	size := fmt.Sprintf("%d/%d", cur.cpu, cur.memMiB)

	diskCur, _ := cfg.Get("disk.size")
	diskVal := diskCur.String()
	diskOpts := options([]string{"20G", "50G", "100G", "200G"}, diskVal)

	agentVal := cfg.String("default_agent")
	agentOpts := options(agents.Builtin().Names(), agentVal)

	idleCur, _ := cfg.Get("idle_timeout")
	idleVal := idleCur.String()
	idleOpts := options([]string{"5m", "10m", "30m", "1h"}, idleVal)

	allowVal := strings.Join(cfg.List("network.allow"), "\n")
	if s, _ := cfg.Get("network.allow"); s.Requested != nil {
		allowVal = strings.Join(rawStrings(s.Requested), "\n")
	}
	ok := true

	form := huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title("Box size").Description(strings.Join(sizeDesc, "\n")).Options(sizeChoices...).Value(&size).
			Validate(func(v string) error {
				if unfit[v] {
					return fmt.Errorf("the machine can't hold this size; pick a smaller one, or use a bigger machine")
				}
				return nil
			})),
		huh.NewGroup(huh.NewSelect[string]().Title("Disk").Description(join("Sparse: only written blocks use space. Changing it applies to new boxes.", reason("disk.size"))).Options(diskOpts...).Value(&diskVal)),
		huh.NewGroup(huh.NewSelect[string]().Title("Agent").Description(join("Started by hutch new without --agent or --cmd.", reason("default_agent"))).Options(agentOpts...).Value(&agentVal)),
		huh.NewGroup(huh.NewSelect[string]().Title("Sleep").Description(join("A box sleeps after this long without activity; waking takes about a second.", reason("idle_timeout"))).Options(idleOpts...).Value(&idleVal)),
		huh.NewGroup(huh.NewText().Title("Network").Description(join("Private host:port targets boxes may reach, one per line. These are requests: each machine's user grants them.", reason("network.allow"))).Value(&allowVal)),
		huh.NewGroup(huh.NewConfirm().Title("Write these settings to .hutch/hutch.toml?").Affirmative("Write").Negative("Cancel").Value(&ok)),
	)
	if err := form.Run(); err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cancelled")
	}

	changes := map[string]any{}
	cpu, mem, _ := strings.Cut(size, "/")
	if n, _ := strconv.Atoi(cpu); n != cur.cpu {
		changes["size.cpu"] = int64(n)
	}
	if n, _ := strconv.Atoi(mem); n != cur.memMiB {
		changes["size.memory"] = sizeValue(n)
	}
	if diskVal != diskCur.String() {
		changes["disk.size"] = diskVal
	}
	if agentVal != cfg.String("default_agent") {
		changes["default_agent"] = agentVal
	}
	if idleVal != idleCur.String() {
		changes["idle_timeout"] = idleVal
	}
	var allow []any
	for _, l := range strings.Split(allowVal, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			allow = append(allow, l)
		}
	}
	if s, _ := cfg.Get("network.allow"); !slices.Equal(rawStrings(allow), rawStrings(orEmpty(s.Requested))) {
		changes["network.allow"] = allow
		if err := checkAllow(rawStrings(allow)); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// sizeValue writes MiB as a config size, like "8G".
func sizeValue(miB int) string {
	if miB%1024 == 0 {
		return fmt.Sprintf("%dG", miB/1024)
	}
	return fmt.Sprintf("%dM", miB)
}

func options(values []string, current string) []huh.Option[string] {
	if !slices.Contains(values, current) {
		values = append(slices.Clone(values), current)
	}
	var out []huh.Option[string]
	for _, v := range values {
		label := v
		if v == current {
			label += "   (current)"
		}
		out = append(out, huh.NewOption(label, v))
	}
	return out
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func rawStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func orEmpty(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

// fitTarget is an online worker a box size is judged against.
type fitTarget struct {
	name         string
	cpus, memMiB int
}

func fitTargets(ctx context.Context, sess *yamux.Session) ([]fitTarget, error) {
	var ws []api.Worker
	if err := rpc.Call(ctx, sess, api.WorkerList, struct{}{}, &ws); err != nil {
		return nil, err
	}
	var out []fitTarget
	for _, w := range ws {
		if w.Online && w.MemMiB > 0 {
			out = append(out, fitTarget{w.Name, w.CPUs, w.MemMiB})
		}
	}
	return out, nil
}

// perWorker is how many boxes of a size fit on a target, after the host's margin.
func (t fitTarget) perWorker(sz boxSize, margin float64) int {
	if sz.cpu > t.cpus {
		return 0
	}
	return int(float64(t.memMiB) * (1 - margin) / float64(sz.memMiB))
}
