package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Projects are tabs above the list, each with the dots of its threads that want
// you. The list shows the open tab's boxes; n starts work in its project. Closing
// a tab hides it on this computer until p brings it back or it has new unread
// work. Choosing a tab never wakes, builds, or creates anything.

// otherTab holds boxes that belong to no project.
const otherTab = "\x00other"

// tabState is what this computer remembers of the tabs.
type tabState struct {
	Active string   `json:"active"`
	Closed []string `json:"closed"`
}

func tabStateFile() string { return filepath.Join(filepath.Dir(projectPathsFile()), "tabs.json") }

func loadTabState() tabState {
	var s tabState
	if data, err := os.ReadFile(tabStateFile()); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

func (m *tui) saveTabs() {
	data, _ := json.Marshal(tabState{Active: m.active, Closed: m.closed})
	os.MkdirAll(filepath.Dir(tabStateFile()), 0o700)
	os.WriteFile(tabStateFile(), data, 0o600)
}

// known is every project: the machine's, those with boxes, and this folder's.
func (m *tui) known() []string {
	seen := map[string]bool{}
	for id := range m.projects {
		seen[id] = true
	}
	for _, b := range m.boxes {
		if b.Kind == api.BoxVersion {
			continue
		}
		if b.Project == "" {
			seen[otherTab] = true
		} else {
			seen[b.Project] = true
		}
	}
	if m.here != "" {
		seen[m.here] = true
	}
	var ids []string
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == m.here) != (ids[j] == m.here) {
			return ids[i] == m.here
		}
		if (ids[i] == otherTab) != (ids[j] == otherTab) {
			return ids[j] == otherTab
		}
		return m.tabName(ids[i]) < m.tabName(ids[j])
	})
	return ids
}

// tabs are the known projects but those closed here.
func (m *tui) tabs() []string {
	return slices.DeleteFunc(m.known(), func(id string) bool { return slices.Contains(m.closed, id) })
}

func (m *tui) tabName(id string) string {
	if id == otherTab {
		return "other"
	}
	return m.projectName(id)
}

// project is the open tab: the one used last, else this folder's, else the first.
func (m *tui) project() string {
	tabs := m.tabs()
	if slices.Contains(tabs, m.active) {
		return m.active
	}
	if slices.Contains(tabs, m.here) {
		return m.here
	}
	if len(tabs) > 0 {
		return tabs[0]
	}
	return ""
}

func (m *tui) inTab(b api.Box) bool {
	p := m.project()
	return b.Project == p || b.Project == "" && p == otherTab
}

func (m *tui) switchTab(id string) {
	if id == m.project() {
		return
	}
	m.closed = slices.DeleteFunc(m.closed, func(c string) bool { return c == id })
	m.active = id
	m.saveTabs()
	m.cursor, m.offset = 0, 0
	m.build()
	m.cursor = 0
	m.sync()
}

func (m *tui) stepTab(d int) {
	tabs := m.tabs()
	if i := slices.Index(tabs, m.project()); i >= 0 && len(tabs) > 1 {
		m.switchTab(tabs[(i+d+len(tabs))%len(tabs)])
	}
}

func (m *tui) closeTab(id string) {
	if id == "" || slices.Contains(m.closed, id) {
		return
	}
	next := m.project()
	if id == next {
		tabs := m.tabs()
		i := slices.Index(tabs, id)
		next = ""
		if len(tabs) > 1 {
			next = tabs[(i+1)%len(tabs)]
		}
	}
	m.closed = append(m.closed, id)
	m.active = next
	m.saveTabs()
	m.build()
	m.cursor = 0
	m.sync()
	m.setStatus(m.tabName(id) + " closed; its boxes keep running. p brings it back")
}

// reopen brings back a closed tab that has new unread work.
func (m *tui) reopen(id string) {
	if slices.Contains(m.closed, id) {
		m.closed = slices.DeleteFunc(m.closed, func(c string) bool { return c == id })
		m.saveTabs()
	}
}

// tabDots counts a project's threads that want you, in the list's colors:
// unread, exited, working.
func (m *tui) tabDots(id string) string {
	var unread, exited, working int
	for _, b := range m.boxes {
		if b.Project != id && !(b.Project == "" && id == otherTab) {
			continue
		}
		for _, t := range b.Threads {
			key := paneKey(b.ID, t.N)
			switch {
			case t.Unread && !m.read[key] && t.State != api.ThreadWorking:
				unread++
			case b.State != "running":
			case t.State == api.ThreadExited && t.Agent != "":
				exited++
			case t.State == api.ThreadWorking:
				working++
			}
		}
	}
	var out string
	for _, d := range []struct {
		n     int
		style lipgloss.Style
	}{{unread, styleUnread}, {exited, styleExited}, {working, styleWorking}} {
		if d.n > 0 {
			out += " " + d.style.Render(fmt.Sprintf("●%d", d.n))
		}
	}
	return out
}

// tabHit is where a tab, or its ×, is on the tab line.
type tabHit struct {
	x0, x1 int
	id     string // "+" for the add button
	close  bool
}

// tabBar draws the tabs, keeping the open one in view; those that don't fit
// collapse into ‹ n and n › counts.
func (m *tui) tabBar(w int) string {
	tabs, active := m.tabs(), m.project()
	m.tabHits = nil
	plus := " " + styleBold.Render("+") + " "
	if len(tabs) == 0 {
		m.tabHits = append(m.tabHits, tabHit{0, w, "+", false})
		return truncate(plus+styleDim.Render(" no projects yet: p adds a checkout"), w)
	}
	cells := make([]string, len(tabs))
	for i, id := range tabs {
		name := m.tabName(id)
		style := styleDim
		if id == active {
			style = styleBold
		}
		if id == m.here && id != active {
			style = lipgloss.NewStyle()
		}
		cells[i] = " " + style.Render(truncate(name, 24)) + m.tabDots(id) + styleDim.Render(" ×") + " "
		if id == active {
			cells[i] = highlight(cells[i], lipgloss.Width(cells[i]), colorCursor)
		}
	}
	room := w - lipgloss.Width(plus) - 8 // the arrows
	a := max(slices.Index(tabs, active), 0)
	lo, hi, used := a, a+1, lipgloss.Width(cells[a])
	for {
		grew := false
		if hi < len(tabs) && used+lipgloss.Width(cells[hi]) <= room {
			used += lipgloss.Width(cells[hi])
			hi, grew = hi+1, true
		}
		if lo > 0 && used+lipgloss.Width(cells[lo-1]) <= room {
			lo--
			used += lipgloss.Width(cells[lo])
			grew = true
		}
		if !grew {
			break
		}
	}
	m.tabHits = append(m.tabHits, tabHit{0, lipgloss.Width(plus), "+", false})
	line := plus
	if lo > 0 {
		left := styleDim.Render(fmt.Sprintf("‹ %d ", lo))
		m.tabHits = append(m.tabHits, tabHit{lipgloss.Width(line), lipgloss.Width(line) + lipgloss.Width(left), tabs[lo-1], false})
		line += left
	}
	for i := lo; i < hi; i++ {
		x := lipgloss.Width(line)
		cw := lipgloss.Width(cells[i])
		m.tabHits = append(m.tabHits, tabHit{x, x + cw - 3, tabs[i], false}, tabHit{x + cw - 3, x + cw, tabs[i], true})
		line += cells[i] + styleRule.Render("│")
	}
	if hi < len(tabs) {
		right := styleDim.Render(fmt.Sprintf(" %d ›", len(tabs)-hi))
		m.tabHits = append(m.tabHits, tabHit{lipgloss.Width(line), lipgloss.Width(line) + lipgloss.Width(right), tabs[hi], false})
		line += right
	}
	return truncate(line, w)
}

// clickTab handles a click on the tab line.
func (m *tui) clickTab(x int) tea.Cmd {
	for _, h := range m.tabHits {
		if x < h.x0 || x >= h.x1 {
			continue
		}
		switch {
		case h.id == "+":
			m.openPicker()
		case h.close:
			m.closeTab(h.id)
		default:
			m.switchTab(h.id)
		}
		return nil
	}
	return nil
}

// The project picker: closed projects to bring back, and adding a checkout.
type picker struct {
	items  []string // project IDs; the last item is adding a checkout
	i      int
	adding bool
	path   textinput.Model
	err    string
}

const addCheckout = "\x00add"

func (m *tui) openPicker() {
	p := &picker{path: textinput.New()}
	for _, id := range m.known() {
		if slices.Contains(m.closed, id) {
			p.items = append(p.items, id)
		}
	}
	p.items = append(p.items, addCheckout)
	p.path.Placeholder = "~/git/my-app"
	p.path.SetWidth(48)
	m.picker = p
}

func (m *tui) pickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	k := msg.String()
	if p.adding {
		switch k {
		case "esc":
			p.adding, p.err = false, ""
			p.path.Blur()
			return nil
		case "enter":
			return m.addProject(p.path.Value())
		}
		var cmd tea.Cmd
		p.path, cmd = p.path.Update(msg)
		return cmd
	}
	switch k {
	case "esc", "q":
		m.picker = nil
	case "up", "k":
		p.i = (p.i + len(p.items) - 1) % len(p.items)
	case "down", "j":
		p.i = (p.i + 1) % len(p.items)
	case "enter":
		if p.items[p.i] == addCheckout {
			p.adding = true
			return p.path.Focus()
		}
		m.picker = nil
		m.switchTab(p.items[p.i])
	}
	return nil
}

// addProject adds an existing checkout as a project: it remembers where it is
// and registers it with the machine, which starts nothing.
func (m *tui) addProject(dir string) tea.Cmd {
	p := m.picker
	dir = strings.TrimSpace(dir)
	if rest, ok := strings.CutPrefix(dir, "~"); ok {
		home, _ := os.UserHomeDir()
		dir = home + rest
	}
	if dir == "" {
		p.err = "type the folder of a git checkout"
		return nil
	}
	repo, err := config.FindRepo(dir)
	switch {
	case err != nil:
		p.err = err.Error()
		return nil
	case repo == nil:
		p.err = dir + " isn't in a git repository"
		return nil
	case strings.HasPrefix(repo.ID, "local:"):
		p.err = "it needs an origin remote: boxes clone the project from there"
		return nil
	}
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		var proj api.Project
		if err := rpc.Call(ctx, sess, api.ProjectInit, api.ProjectParams{Project: repo.ID}, &proj); err != nil {
			return addedMsg{err: err}
		}
		rememberProject(repo.ID, repo.Root)
		return addedMsg{project: proj}
	}
}

type addedMsg struct {
	project api.Project
	err     error
}

func (m *tui) added(msg addedMsg) tea.Cmd {
	if msg.err != nil {
		if m.picker != nil {
			m.picker.err = msg.err.Error()
		}
		return nil
	}
	m.picker = nil
	m.projects[msg.project.ID] = msg.project.Slug
	m.switchTab(msg.project.ID)
	return m.fetchTemplates(msg.project.ID)
}

func (m *tui) pickerView() string {
	p := m.picker
	w := min(64, m.w-4)
	lines := []string{styleBold.Render("Projects"), ""}
	if p.adding {
		lines = append(lines, "Folder of a git checkout (with an origin remote):", "", p.path.View())
		if p.err != "" {
			lines = append(lines, "", styleError.Render(truncate(p.err, w-6)))
		}
		lines = append(lines, "", styleDim.Render("enter adds it · esc goes back"))
		return stylePopup.Width(w).Render(strings.Join(lines, "\n"))
	}
	for i, id := range p.items {
		label := m.tabName(id) + styleDim.Render(" (closed)") + m.tabDots(id)
		if id == addCheckout {
			label = "+ Add a checkout…"
		}
		if i == p.i {
			lines = append(lines, styleLabel.Render("▸ ")+label)
		} else {
			lines = append(lines, "  "+label)
		}
	}
	lines = append(lines, "", styleDim.Render("enter opens · esc closes"))
	return stylePopup.Width(w).Render(strings.Join(lines, "\n"))
}
