package main

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/mouizahmed/hutch/internal/api"
)

// Search narrows the open tab's list to boxes and terminals whose names, titles,
// agents, branches, or machines hold every word typed. It wakes, starts, and
// changes nothing.

type searchState struct {
	input  textinput.Model
	typing bool
	query  string
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
		m.clearSearch()
		return nil
	case "enter":
		m.search.typing = false
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

func (m *tui) clearSearch() {
	m.search = searchState{}
	m.build()
	m.sync()
}

func (m *tui) boxMatches(b api.Box) bool {
	return m.searchHits(b.ID, b.Branch, b.Machine, m.workerName(b.Worker))
}

func (m *tui) threadMatches(b api.Box, t api.Thread) bool {
	return m.searchHits(t.Title, t.Agent, b.ID, b.Branch, b.Machine, m.workerName(b.Worker))
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
