package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// A project's template in the TUI: its row (the template box while one exists),
// a terminal on it (N), build (b), snapshot (S), and setting the project up (i).
// Builds stay with git: b and S bring the template box's work back, and commit
// and push .hutch/ first, through hutch template build/snapshot --commit.

type templatesMsg struct {
	project  string
	versions []api.Template
}

func (m *tui) fetchTemplates(project string) tea.Cmd {
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		var vs []api.Template
		if rpc.Call(ctx, sess, api.TemplateList, api.ProjectParams{Project: project}, &vs) != nil {
			return nil
		}
		return templatesMsg{project, vs}
	}
}

// templateLabel is a project's template as its row reads: its newest version.
func (m *tui) templateLabel(project string) string {
	if vs := m.templates[project]; len(vs) > 0 {
		return fmt.Sprintf("template v%d", vs[0].Version)
	}
	return "template"
}

// templateOf is the project whose template a row is, or "".
func templateOf(r tuiRow) string {
	switch {
	case r.kind == rowTemplate:
		return r.project
	case r.box.Kind == api.BoxTemplate:
		return r.box.Project
	}
	return ""
}

// templateKey handles N, i, b, and S on a template's row.
func (m *tui) templateKey(project, key string) tea.Cmd {
	dir := m.checkout(project)
	if dir == "" {
		m.setStatus(m.projectName(project) + ": checkout needed here: p → Add a checkout")
		return nil
	}
	name := m.projectName(project)
	if key == "i" || m.projects[project] == "" || !m.hasRecipe(project) {
		if key != "i" {
			m.setStatus(name + " isn't set up yet: i runs hutch init, which starts a template box and writes a setup prompt")
			return nil
		}
		t := m.run(dir, "set up "+name, "", "init")
		m.tasks[len(m.tasks)-1].report = []string{"Project", "Next:", "  1."}
		m.tasks[len(m.tasks)-1].after = m.fetchProjects()
		return t
	}
	verb := map[string]string{"b": "build", "S": "snapshot"}[key]
	switch {
	case key == "N":
		return m.openForm("template:" + project)
	case verb == "":
	case !m.tmplInfo[project].confirmed:
		// The CLI asks for the size first, so it gets the terminal.
		return m.interactive(dir, "template", verb)
	default:
		return m.checkTemplate(project, dir, verb)
	}
	return nil
}

// hasRecipe reports whether a project has been set up: a template version, or
// a .hutch/ in its checkout.
func (m *tui) hasRecipe(project string) bool {
	if len(m.templates[project]) > 0 {
		return true
	}
	dir := m.checkout(project)
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".hutch", "Dockerfile"))
	return err == nil
}

func (m *tui) checkout(project string) string {
	if project == m.here {
		return m.cwd
	}
	return projectDir(project, m.boxes)
}

// checkTemplate brings the template box's work here, in the background, for b
// or S to review before the CLI does what it says.
func (m *tui) checkTemplate(project, dir, verb string) tea.Cmd {
	m.setStatus("checking the " + m.projectName(project) + " template box…")
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		r, err := readRecipe(ctx, sess, project, dir)
		return templateWorkMsg{project, verb, r, err}
	}
}

type templateWorkMsg struct {
	project, verb string
	recipe        recipe
	err           error
}

func (m *tui) reviewTemplate(msg templateWorkMsg) tea.Cmd {
	m.status = ""
	if msg.err != nil {
		m.setStatus("template: " + msg.err.Error())
		return nil
	}
	if msg.recipe.ready() {
		return m.templateTask(msg.project, msg.recipe.root, msg.verb)
	}
	m.confirm = msg.verb + " " + msg.project
	m.confirmDir = msg.recipe.root
	m.confirmText = msg.recipe.review()
	if msg.verb == "snapshot" {
		m.confirmText = strings.TrimSuffix(m.confirmText, "build that commit?") + "snapshot that commit?"
	}
	m.confirmHint = "y goes ahead · any other key cancels"
	return nil
}

// templateTask runs hutch template build or snapshot; --commit does what the
// review said, so the CLI is the one implementation of it.
func (m *tui) templateTask(project, dir, verb string) tea.Cmd {
	name := m.projectName(project)
	t := m.run(dir, verb+" the "+name+" template", "", "template", verb, "--commit")
	// A build measures the project's peaks; a snapshot adds a version.
	m.tasks[len(m.tasks)-1].after = m.fetchProjects()
	if verb == "snapshot" {
		m.tasks[len(m.tasks)-1].report = []string{"Template v", "Built and verified"}
		m.tasks[len(m.tasks)-1].after = tea.Batch(m.fetchTemplates(project), m.fetchProjects())
	} else {
		m.tasks[len(m.tasks)-1].report = []string{"Built and verified", "Peak use"}
	}
	return t
}

// templateInfo is what the template's row reads from the checkout and the
// project config, when it's selected; nothing in a box is read or woken.
type templateInfo struct {
	changed   int // .hutch/ files changed here, uncommitted
	since     int // commits changing .hutch/ since the newest version; -1 unknown
	cpu       int
	memMiB    int
	disk      string
	confirmed bool
	err       string
}

type templateInfoMsg struct {
	project string
	info    templateInfo
}

// loadTemplateInfo reads project's templateInfo in the background, once each
// time its row is selected or something about it changed.
func (m *tui) loadTemplateInfo(project string) {
	if m.offline || m.tmplFor == project {
		return
	}
	m.tmplFor = project
	dir, confirmed, prog, ctx, sess := m.checkout(project), m.projectInfo[project].Confirmed, m.prog, m.ctx, m.sess
	newest := ""
	if vs := m.templates[project]; len(vs) > 0 {
		newest = vs[0].Commit
	}
	if dir == "" {
		return
	}
	go func() { prog.Send(templateInfoMsg{project, readTemplateInfo(ctx, sess, dir, newest, confirmed)}) }()
}

func readTemplateInfo(ctx context.Context, sess *yamux.Session, dir, newest, confirmed string) templateInfo {
	info := templateInfo{}
	info.changed, info.since = recipeChanges(dir, newest)
	cfg, err := loadConfigIn(ctx, sess, "", dir)
	if err != nil {
		info.err = err.Error()
		return info
	}
	info.cpu, info.memMiB = int(cfg.Int("size.cpu")), int(cfg.Size("size.memory")>>20)
	info.disk = sizeValue(int(cfg.Size("disk.size") >> 20))
	info.confirmed = confirmed != "" && settingsDigest(cfg) == confirmed
	return info
}

// waitingThread is the terminal Enter opens on a template box's row: an agent
// waiting for you, else the newest; none without terminals.
func waitingThread(b api.Box) api.Thread {
	for _, t := range b.Threads {
		if t.Agent != "" && t.State == api.ThreadWaiting {
			return t
		}
	}
	if len(b.Threads) > 0 {
		return b.Threads[len(b.Threads)-1]
	}
	return api.Thread{}
}

// recipeChanges counts the recipe's uncommitted changes in dir, and its commits
// since newest (-1 when that commit isn't there or there's no version).
func recipeChanges(dir, newest string) (changed, since int) {
	if out, err := git(dir, append([]string{"status", "--porcelain", "--"}, recipePaths...)...); err == nil && out != "" {
		changed = len(strings.Split(out, "\n"))
	}
	since = -1
	if newest != "" {
		if out, err := git(dir, append([]string{"rev-list", "--count", newest + "..HEAD", "--"}, recipePaths...)...); err == nil {
			since, _ = strconv.Atoi(out)
		}
	}
	return changed, since
}

// templatePane is the template's row: where the project's template stands, and
// the one key that takes it on from there.
func (m *tui) templatePane(project string, tb *api.Box, w int) string {
	name, dir, vs := m.projectName(project), m.checkout(project), m.templates[project]
	ok, bad, dim := styleRead.Render("✓"), styleError.Render("!"), styleDim.Render("·")
	head := styleBold.Render("The " + name + " template")
	if len(vs) > 0 {
		head += styleDim.Render(fmt.Sprintf("  v%d · %s · new boxes start from it", vs[0].Version, ago(vs[0].Created)))
	} else {
		head += styleDim.Render("  no version yet")
	}
	lines := []string{"", " " + head, ""}
	line := func(mark, what, text string) {
		lines = append(lines, truncate(fmt.Sprintf("  %s %-13s %s", mark, what, text), w-1))
	}
	setUp := m.projects[project] != "" && m.hasRecipe(project)
	info, loaded := m.tmplInfo[project]
	next := ""
	switch {
	case dir == "":
		line(bad, "checkout", "none here")
		next = "p → Add a checkout: the template is built from one"
	case !setUp:
		line(bad, "set up", "no .hutch/ yet")
		next = "i sets it up: hutch init starts the template box and writes a setup prompt for an agent"
	default:
		line(ok, "set up", ".hutch/ in "+dir)
	}
	if setUp && dir != "" {
		switch {
		case tb == nil:
			line(dim, "template box", "none: N starts one with a terminal")
		case tb.State == "failed":
			line(bad, "template box", tb.ID+" failed and can't be read; b builds a new one")
		case tb.State != "running":
			line(dim, "template box", tb.ID+" is "+tb.State)
		default:
			var ts []string
			for _, t := range tb.Threads {
				ts = append(ts, fmt.Sprintf("%d %s %s", t.N, threadAgent(t), threadState(*tb, t)))
			}
			if len(ts) == 0 {
				ts = []string{"no terminals"}
			}
			line(ok, "template box", tb.ID+" awake · "+strings.Join(ts, ", "))
		}
		switch {
		case !loaded:
			line(dim, "recipe", "reading…")
		case info.changed > 0 || info.since > 0:
			var what []string
			if info.changed > 0 {
				what = append(what, fmt.Sprintf("%d uncommitted change(s)", info.changed))
			}
			if info.since > 0 {
				what = append(what, fmt.Sprintf("%d commit(s) since v%d", info.since, vs[0].Version))
			}
			line(bad, "recipe", strings.Join(what, " · "))
		case len(vs) == 0:
			line(bad, "recipe", "not built yet")
		case info.since < 0:
			line(dim, "recipe", fmt.Sprintf("v%d's commit isn't here; b checks it", vs[0].Version))
		default:
			line(ok, "recipe", fmt.Sprintf("as v%d", vs[0].Version))
		}
		if tb != nil && tb.State != "failed" {
			line(dim, "", "work in the template box comes back with b or S, for review first")
		}
		switch {
		case !loaded:
		case info.err != "":
			line(bad, "size", info.err)
		case !info.confirmed:
			line(bad, "size", fmt.Sprintf("%d vCPU · %s · %s disk, not confirmed", info.cpu, gb(info.memMiB), info.disk))
		default:
			line(ok, "size", fmt.Sprintf("%d vCPU · %s · %s disk", info.cpu, gb(info.memMiB), info.disk))
		}
		if p := m.projectInfo[project].Peaks; p != nil && p.OfVCPUs > 0 {
			line(" ", "", fmt.Sprintf("the last build used at most %.1f of %d vCPUs and %s of %s", p.CPUs, p.OfVCPUs, gb(p.MemMiB), gb(p.OfMiB)))
		}
		switch {
		case !loaded:
		case !info.confirmed:
			next = "b or S first asks you to confirm the size (the project form), then builds"
		case len(vs) == 0 || info.changed > 0 || info.since != 0:
			next = "b builds and checks the recipe · S makes it the next version"
		default:
			next = "up to date · N opens a terminal on the template to change it"
		}
		if tb != nil && tb.State == "running" {
			if t := waitingThread(*tb); t.Agent != "" && t.State == api.ThreadWaiting {
				next = fmt.Sprintf("Enter opens terminal %d: %s is waiting for you · then %s", t.N, t.Agent, next)
			}
		}
	}
	if next != "" {
		lines = append(lines, "", " "+styleBold.Render("next")+"  "+next)
	}
	lines = append(lines, "", styleDim.Render("  N a terminal on the template · b build · S snapshot · i set up · Space more"))
	return strings.Join(lines, "\n")
}
