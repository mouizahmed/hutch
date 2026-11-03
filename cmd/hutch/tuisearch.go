package main

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/mouizahmed/hutch/internal/api"
)

// Search narrows the list to boxes and terminals whose names, titles, agents,
// branches, or machines hold every word typed; tab widens it to every project.
// Filters narrow it by state, unread work, or machine. Neither wakes, starts,
// or changes anything, and both say when they hide everything.

type searchState struct {
	input  textinput.Model
	typing bool
	query  string
	all    bool // every project, not just the open tab
}

type filterState struct {
	state   string // working, waiting, exited, unread, awake, asleep, unreachable
	machine string
}

func (m *tui) startSearch() tea.Cmd {
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "box, terminal, agent, branch, machine"
	in.SetValue(m.search.query)
	m.search.input, m.search.typing = in, true
	return m.search.input.Focus()
}

func (m *tui) searchKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.search = searchState{}
		m.build()
		m.sync()
		return nil
	case "enter":
		m.search.typing = false
		m.jumpToSelection()
		return nil
	case "tab":
		m.search.all = !m.search.all
		m.build()
		m.sync()
		return nil
	case "down", "ctrl+n":
		m.move(1)
		return nil
	case "up", "ctrl+p":
		m.move(-1)
		return nil
	}
	var cmd tea.Cmd
	m.search.input, cmd = m.search.input.Update(msg)
	m.search.query = strings.TrimSpace(m.search.input.Value())
	m.build()
	m.sync()
	return cmd
}

// jumpToSelection opens the tab of a box found in another project, keeping it
// selected.
func (m *tui) jumpToSelection() {
	r := m.selected()
	if !m.search.all || r.box.ID == "" {
		return
	}
	project := r.box.Project
	if project == "" {
		project = otherTab
	}
	key := r.key()
	m.search = searchState{}
	m.switchTab(project)
	m.build()
	m.selectKey(key)
	m.sync()
}

func (m *tui) filterMenu() tea.Cmd {
	set := func(state, machine string) func() tea.Cmd {
		return func() tea.Cmd {
			m.filter = filterState{state: state, machine: machine}
			m.build()
			m.sync()
			return nil
		}
	}
	items := []menuItem{
		{label: "Unread", detail: "terminals with work you haven't seen", do: set("unread", "")},
		{label: "Working", detail: "terminals busy now", do: set("working", "")},
		{label: "Waiting", detail: "terminals waiting for you", do: set("waiting", "")},
		{label: "Exited", detail: "terminals whose program ended", do: set("exited", "")},
		{label: "Awake", detail: "running boxes", do: set("awake", "")},
		{label: "Asleep", detail: "sleeping or archived boxes", do: set("asleep", "")},
		{label: "Unreachable", detail: "boxes on machines that are offline", do: set("unreachable", "")},
	}
	for _, w := range m.liveWorkers() {
		items = append(items, menuItem{label: "On " + w.Name, detail: "boxes on this machine", do: set("", w.Name)})
	}
	items = append(items, menuItem{label: "No filter", detail: "everything", do: set("", "")})
	m.choose("Filter the list", items)
	return nil
}

func (m *tui) noFilters() string {
	if m.search.query == "" && m.filter == (filterState{}) {
		return "no search or filter is on"
	}
	return ""
}

func (m *tui) clearFilters() {
	m.search, m.filter = searchState{}, filterState{}
	m.build()
	m.sync()
}

// filtering reports whether a search or filter narrows the list.
func (m *tui) filtering() bool { return m.search.query != "" || m.filter != (filterState{}) }

// unreachable reports whether a box's machine is offline.
func (m *tui) unreachable(b api.Box) bool {
	for _, w := range m.workers {
		if w.ID == b.Worker {
			return !w.Online
		}
	}
	return false
}

// boxMatches reports whether a box itself passes the search and filter; with
// threadMatches, a box shows when it or any of its terminals does.
func (m *tui) boxMatches(b api.Box) bool {
	f := m.filter
	if f.machine != "" && b.Machine != f.machine && m.workerName(b.Worker) != f.machine {
		return false
	}
	switch f.state {
	case "awake":
		if b.State != "running" {
			return false
		}
	case "asleep":
		if b.State != "asleep" && b.State != "archived" {
			return false
		}
	case "unreachable":
		if !m.unreachable(b) {
			return false
		}
	case "working", "waiting", "exited", "unread":
		return false // a terminal's state: the box shows through its terminals
	}
	return m.searchHits(b.ID, b.Branch, b.Machine, m.projectName(b.Project))
}

func (m *tui) threadMatches(b api.Box, t api.Thread) bool {
	f := m.filter
	if f.machine != "" && b.Machine != f.machine && m.workerName(b.Worker) != f.machine {
		return false
	}
	switch f.state {
	case "working":
		if t.State != api.ThreadWorking {
			return false
		}
	case "waiting":
		if t.State != api.ThreadWaiting {
			return false
		}
	case "exited":
		if t.State != api.ThreadExited {
			return false
		}
	case "unread":
		if !t.Unread || m.read[paneKey(b.ID, t.N)] {
			return false
		}
	case "awake", "asleep", "unreachable":
		if !m.boxMatches(b) {
			return false
		}
	}
	return m.searchHits(t.Title, t.Agent, b.ID, b.Branch, b.Machine)
}

// searchHits reports whether every word of the query is in one of fields.
func (m *tui) searchHits(fields ...string) bool {
	words := strings.Fields(strings.ToLower(m.search.query))
	hay := strings.ToLower(strings.Join(fields, " "))
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

func (m *tui) workerName(id string) string {
	for _, w := range m.workers {
		if w.ID == id {
			return w.Name
		}
	}
	return ""
}

// filterLine says what narrows the list, for the top of it.
func (m *tui) filterLine() string {
	var parts []string
	if m.search.query != "" {
		scope := ""
		if m.search.all {
			scope = " in every project"
		}
		parts = append(parts, "search "+`"`+m.search.query+`"`+scope)
	}
	if m.filter.state != "" {
		parts = append(parts, m.filter.state)
	}
	if m.filter.machine != "" {
		parts = append(parts, "on "+m.filter.machine)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ") + " (F clears)"
}
