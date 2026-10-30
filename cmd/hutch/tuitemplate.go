package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

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
	switch key {
	case "N":
		return m.openForm("template:" + project)
	case "S":
		return m.checkTemplate(project, dir, "snapshot")
	case "b":
		return m.checkTemplate(project, dir, "build")
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
	if verb == "snapshot" {
		m.tasks[len(m.tasks)-1].report = []string{"Template v", "Built and verified"}
		m.tasks[len(m.tasks)-1].after = m.fetchTemplates(project)
	} else {
		m.tasks[len(m.tasks)-1].report = []string{"Built and verified", "Peak use"}
	}
	return t
}
