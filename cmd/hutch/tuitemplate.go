package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// A project's template in the TUI: its row (the template box while one exists),
// a terminal on it (N), build (b), snapshot (S), and setting the project up (i).
// Builds stay with git: b brings the template box's work back, and commits and
// pushes .hutch/ first.

type templatesMsg struct {
	project  string
	versions []api.Template
}

const templateCommit = "Update the Hutch template"

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

// templateKey handles N, t, b, and S on a template's row.
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
	switch key {
	case "N":
		return m.openForm("template:" + project)
	case "S":
		t := m.run(dir, "snapshot the "+name+" template", "", "template", "snapshot")
		m.tasks[len(m.tasks)-1].report = []string{"Template v", "Built and verified"}
		m.tasks[len(m.tasks)-1].after = m.fetchTemplates(project)
		return t
	case "b":
		return m.checkTemplate(project, dir)
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

// checkTemplate brings the template box's work here, in the background, for b to
// review.
func (m *tui) checkTemplate(project, dir string) tea.Cmd {
	m.setStatus("checking the " + m.projectName(project) + " template box…")
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		commits, err := templateWork(ctx, sess, project, dir)
		return templateWorkMsg{project, dir, commits, err}
	}
}

type templateWorkMsg struct {
	project, dir string
	commits      []string
	err          error
}

// reviewTemplate asks before b changes anything: .hutch/ changed here, work in
// the template box (on hutch/template), or commits not on origin are committed,
// merged, and pushed first, so the build is of what you saw.
func (m *tui) reviewTemplate(msg templateWorkMsg) tea.Cmd {
	m.status = ""
	if msg.err != nil {
		m.setStatus("template box: " + msg.err.Error())
		return nil
	}
	branch, _ := git(msg.dir, "symbolic-ref", "--short", "-q", "HEAD")
	changed, _ := git(msg.dir, append([]string{"status", "--porcelain", "--"}, recipePaths...)...)
	ahead, err := git(msg.dir, "rev-list", "--count", "@{upstream}..HEAD")
	unpushed := err != nil || ahead != "0"
	if branch == "" {
		m.setStatus(msg.dir + " is on a detached HEAD; check out a branch to build from")
		return nil
	}
	if changed == "" && len(msg.commits) == 0 && !unpushed {
		return m.buildTemplate(msg.project, msg.dir)
	}
	var lines, steps []string
	if changed != "" {
		lines = append(lines, ".hutch/ has changes here:")
		for _, l := range strings.Split(changed, "\n") {
			lines = append(lines, "  "+strings.TrimSpace(l))
		}
		steps = append(steps, fmt.Sprintf("commit them on %s as %q", branch, templateCommit))
	}
	if len(msg.commits) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "The template box has work you don't:")
		for _, c := range msg.commits {
			lines = append(lines, "  "+c)
		}
		steps = append(steps, "merge it into "+branch)
	}
	if len(lines) == 0 {
		lines = append(lines, branch+" has commits not on origin.")
	}
	steps = append(steps, "push", "build that commit?")
	steps[0] = strings.ToUpper(steps[0][:1]) + steps[0][1:]
	lines = append(lines, "", strings.Join(steps, ", then "))
	m.confirm = "build " + msg.project
	m.confirmDir = msg.dir
	m.confirmText = strings.Join(lines, "\n")
	m.confirmHint = "y goes ahead · any other key cancels"
	return nil
}

// recipePaths is what b commits: .hutch/, but hutch.toml may stay uncommitted.
var recipePaths = []string{".hutch", ":!.hutch/hutch.toml"}

// buildTemplate commits .hutch/ changes, merges the template box's work, pushes,
// and builds the result. A merge that conflicts is undone and stops it before the
// build, so the template box keeps its work.
func (m *tui) buildTemplate(project, dir string) tea.Cmd {
	paths := "'" + strings.Join(recipePaths, "' '") + "'"
	script := `git add -- ` + paths + ` && { git diff --cached --quiet -- ` + paths + ` || git commit -q -m "$1" -- ` + paths + `; } &&
if git rev-parse -q --verify "refs/heads/$3" >/dev/null; then
  git merge -q --no-edit "$3" || { git merge --abort 2>/dev/null; echo "The template box's work doesn't merge with yours; nothing was merged and the template box is kept. Merge it yourself (git merge $3), then build again." >&2; exit 1; }
fi && git push -q -u origin HEAD && exec "$2" template build`
	t := m.start(dir, "build the "+m.projectName(project)+" template", "", exec.Command("sh", "-c", script, "sh", templateCommit, m.exe, templateBranch))
	m.tasks[len(m.tasks)-1].report = []string{"Built and verified", "Peak use"}
	return t
}
