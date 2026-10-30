package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mouizahmed/hutch/internal/names"
)

// The task form is the popup n and N open: the agent, its model, the box (a new
// one, or an existing one for a new thread), and a multi-line prompt.

const (
	fieldAgent = iota
	fieldModel
	fieldBox
	fieldName
	fieldPrompt
	fieldCount
)

type taskForm struct {
	field  int
	boxes  []string // "" is a new box
	box    int
	model  textinput.Model
	name   textinput.Model // a new box's name; empty for a generated one
	prompt textarea.Model
	err    string
	// project and dir are where a new box goes.
	project, dir string
}

var (
	stylePopup = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("244")).Padding(1, 2)
	styleLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
)

// openForm opens the task form, on a new box or on box.
func (m *tui) openForm(box string) tea.Cmd {
	f := &taskForm{field: fieldPrompt, boxes: []string{""}}
	f.project, f.dir = m.newProject(m.selected())
	for _, b := range m.boxes {
		if b.Kind == "" {
			f.boxes = append(f.boxes, b.ID)
		}
	}
	// The selected box comes right after "new box".
	if i := slices.Index(f.boxes, box); i > 0 {
		f.boxes = append([]string{"", box}, slices.Delete(f.boxes, i, i+1)[1:]...)
		f.box = 1
	}
	f.model = textinput.New()
	f.model.Prompt = ""
	f.model.CharLimit = 100
	f.model.Placeholder = "default"
	f.model.SetValue(m.models[m.agent])
	f.name = textinput.New()
	f.name.Prompt = ""
	f.name.CharLimit = 63
	f.name.Placeholder = "generated"
	f.prompt = textarea.New()
	f.prompt.ShowLineNumbers = false
	f.prompt.Prompt = ""
	f.prompt.Placeholder = "What should the agent do?"
	f.prompt.CharLimit = 0
	f.prompt.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	if project, ok := strings.CutPrefix(box, "template:"); ok {
		f.boxes, f.box, f.project, f.dir = []string{box}, 0, project, m.checkout(project)
	}
	m.form = f
	m.sizeForm()
	return f.prompt.Focus()
}

func (m *tui) formWidth() int { return min(80, m.w-4) }

func (m *tui) sizeForm() {
	if m.form == nil {
		return
	}
	inner := m.formWidth() - 6
	m.form.prompt.SetWidth(inner - 2)
	m.form.prompt.SetHeight(min(6, max(m.h-17, 2)))
	m.form.name.SetWidth(40)
}

func (m *tui) formKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.form
	k := msg.String()
	switch k {
	case "esc":
		m.form = nil
		return nil
	case "enter":
		return m.submitForm()
	case "tab", "shift+tab":
		return m.formFocus(f.field, map[bool]int{true: 1, false: -1}[k == "tab"])
	case "up", "down":
		if f.field != fieldPrompt || k == "up" && f.prompt.Line() == 0 || k == "down" && f.prompt.Line() == f.prompt.LineCount()-1 {
			return m.formFocus(f.field, map[bool]int{true: 1, false: -1}[k == "down"])
		}
	case "left", "right":
		d := map[bool]int{true: 1, false: -1}[k == "right"]
		switch f.field {
		case fieldAgent:
			m.agent = m.nextAgent(d > 0)
			f.model.SetValue(m.models[m.agent])
			return nil
		case fieldModel:
			m.models[m.agent] = m.stepModel(d)
			f.model.SetValue(m.models[m.agent])
			f.model.CursorEnd()
			return nil
		case fieldBox:
			f.box = (f.box + d + len(f.boxes)) % len(f.boxes)
			return nil
		}
	}
	var cmd tea.Cmd
	switch f.field {
	case fieldModel:
		f.model.SetWidth(lipgloss.Width(f.model.Value()) + 2)
		f.model, cmd = f.model.Update(msg)
		m.models[m.agent] = strings.TrimSpace(f.model.Value())
	case fieldName:
		f.name, cmd = f.name.Update(msg)
		f.err = m.checkName(f.name.Value())
	case fieldPrompt:
		f.prompt, cmd = f.prompt.Update(msg)
	}
	return cmd
}

var boxName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// checkName says what's wrong with a new box's name, if anything.
func (m *tui) checkName(name string) string {
	switch {
	case name == "":
		return ""
	case !boxName.MatchString(name):
		return "use lowercase letters, digits, and dashes (not first)"
	}
	for _, b := range m.boxes {
		if b.ID == name {
			return name + " is taken"
		}
	}
	return ""
}

// fitModel sizes the model field to its text, with room for the cursor.
func fitModel(f *taskForm) {
	width := lipgloss.Width(f.model.Value())
	if width == 0 {
		width = len(f.model.Placeholder)
	}
	f.model.SetWidth(width + 1)
}

func (m *tui) formPaste(msg tea.PasteMsg) tea.Cmd {
	var cmd tea.Cmd
	switch m.form.field {
	case fieldModel:
		m.form.model, cmd = m.form.model.Update(msg)
		m.models[m.agent] = strings.TrimSpace(m.form.model.Value())
	case fieldName:
		m.form.name, cmd = m.form.name.Update(msg)
		m.form.err = m.checkName(m.form.name.Value())
	case fieldPrompt:
		m.form.prompt, cmd = m.form.prompt.Update(msg)
	}
	return cmd
}

// formFocus moves d fields from field i, past fields that don't apply: the model
// for an agent without one, the name for an existing box.
func (m *tui) formFocus(i, d int) tea.Cmd {
	f := m.form
	skip := func(i int) bool {
		return i == fieldModel && !m.hasModels() || i == fieldName && f.boxes[f.box] != ""
	}
	i = (i + d + fieldCount) % fieldCount
	for d != 0 && skip(i) {
		i = (i + d + fieldCount) % fieldCount
	}
	f.field = i
	f.prompt.Blur()
	f.model.Blur()
	f.name.Blur()
	switch i {
	case fieldName:
		return f.name.Focus()
	case fieldPrompt:
		return f.prompt.Focus()
	case fieldModel:
		f.model.CursorEnd()
		return f.model.Focus()
	}
	return nil
}

func (m *tui) hasModels() bool {
	a := m.adapters[m.agent]
	return a != nil && a.Model != ""
}

// stepModel is the agent's suggestion d away from its current model: its default
// first, then its adapter's list; a model typed in steps from the start.
func (m *tui) stepModel(d int) string {
	a := m.adapters[m.agent]
	if a == nil {
		return ""
	}
	options := append([]string{""}, a.Models...)
	i := slices.Index(options, m.models[m.agent])
	if i < 0 && d < 0 {
		i = 0
	}
	return options[(i+d+len(options))%len(options)]
}

func (m *tui) submitForm() tea.Cmd {
	f := m.form
	prompt := strings.TrimSpace(f.prompt.Value())
	box := f.boxes[f.box]
	switch {
	case prompt == "":
		f.err = "write what the agent should do"
		return m.formFocus(fieldPrompt, 0)
	case box == "" && f.dir == "":
		f.err = "a new box needs a project: select one of its boxes first, or run hutch in its checkout"
		return nil
	case box == "" && m.checkName(f.name.Value()) != "":
		f.err = m.checkName(f.name.Value())
		return m.formFocus(fieldName, 0)
	}
	m.form = nil
	args := []string{"--agent", m.agent}
	if model := m.models[m.agent]; model != "" && m.hasModels() {
		args = append(args, "--model", model)
	}
	if project, ok := strings.CutPrefix(box, "template:"); ok {
		return m.run(f.dir, "agent on the "+m.projectName(project)+" template", "", append(append([]string{"thread", "template"}, args...), prompt)...)
	}
	if box == "" {
		taken := map[string]bool{}
		for _, b := range m.boxes {
			taken[b.ID] = true
		}
		name := f.name.Value()
		if name == "" {
			name = names.New(taken)
		}
		return m.run(f.dir, "new box "+name, name+"/1", append(append([]string{"new", "--name", name}, args...), prompt)...)
	}
	next, dir := 1, m.cwd
	for _, b := range m.boxes {
		if b.ID != box {
			continue
		}
		for _, t := range b.Threads {
			next = max(next, t.N+1)
		}
		if b.Project != "" && b.Project != m.here {
			if d := projectDir(b.Project, m.boxes); d != "" {
				dir = d
			}
		}
	}
	return m.run(dir, "new thread in "+box, paneKey(box, next), append(append([]string{"thread", box}, args...), prompt)...)
}

// formView draws the task form.
func (m *tui) formView() string {
	f := m.form
	w := m.formWidth() - 6
	box := f.boxes[f.box]
	title := "New task in " + m.projectName(f.project)
	if project, ok := strings.CutPrefix(box, "template:"); ok {
		title = "New agent on the " + m.projectName(project) + " template"
	} else if box != "" {
		title = "New thread in " + box
	} else if f.project == "" {
		title = "New task"
	}
	row := func(field int, label, value string) string {
		l := styleDim.Render(fmt.Sprintf("  %-7s", label))
		if f.field == field {
			l = styleLabel.Render(fmt.Sprintf("▸ %-7s", label))
		}
		return l + " " + value
	}
	// choice shows the current option between arrows, then the others dim.
	choice := func(field int, options []string, cur int, show func(string) string) string {
		s := "‹ " + styleBold.Render(show(options[cur])) + " ›"
		if f.field != field {
			s = "  " + show(options[cur]) + "  "
		}
		var rest []string
		for i := 1; i < len(options); i++ {
			rest = append(rest, show(options[(cur+i)%len(options)]))
		}
		return s + styleDim.Render(truncate(" "+strings.Join(rest, "  "), max(w-lipgloss.Width(s)-10, 0)))
	}
	agentNames := m.adapters.Names()
	agentRow := choice(fieldAgent, agentNames, max(slices.Index(agentNames, m.agent), 0), func(s string) string { return s })
	var modelRow string
	switch {
	case !m.hasModels():
		modelRow = styleDim.Render("  " + m.agent + " has no model setting")
	case f.field == fieldModel:
		fitModel(f)
		var others []string
		for _, s := range m.adapters[m.agent].Models {
			if s != m.models[m.agent] {
				others = append(others, s)
			}
		}
		modelRow = "‹ " + f.model.View() + " ›" + styleDim.Render(truncate(" "+strings.Join(others, "  "), max(w-40, 0)))
	default:
		cur := m.models[m.agent]
		if cur == "" {
			cur = "default"
		}
		modelRow = "  " + cur
	}
	boxRow := choice(fieldBox, f.boxes, f.box, func(s string) string {
		if s == "" {
			return "new box"
		}
		if strings.HasPrefix(s, "template:") {
			return "template"
		}
		return s
	})
	border := lipgloss.Color("240")
	if f.field == fieldPrompt {
		border = lipgloss.Color("39")
	}
	prompt := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Render(f.prompt.View())
	hint := "enter start · shift+enter newline · tab fields · ←→ change · esc"
	if f.field == fieldModel {
		hint = "←→ suggestions · or type a model · enter start · esc"
	}
	lines := []string{styleBold.Render(title), "", row(fieldAgent, "Agent", agentRow), row(fieldModel, "Model", modelRow),
		row(fieldBox, "Box", boxRow)}
	if box == "" {
		nameRow := "  " + f.name.View()
		if f.field == fieldName {
			nameRow = "‹ " + f.name.View() + " ›"
		} else if f.name.Value() == "" {
			nameRow = styleDim.Render("  generated")
		}
		lines = append(lines, row(fieldName, "Name", nameRow))
	}
	lines = append(lines, "", prompt)
	if f.err != "" {
		lines = append(lines, styleError.Render(truncate(f.err, w)))
	}
	lines = append(lines, "", styleDim.Render(truncate(hint, w)))
	return stylePopup.Width(m.formWidth()).Render(strings.Join(lines, "\n"))
}

// confirmView is the popup d opens.
func (m *tui) confirmView() string {
	w := min(64, m.w-4)
	var lines []string
	for _, l := range strings.Split(m.confirmText, "\n") {
		lines = append(lines, truncate(l, w-6))
	}
	body := styleBold.Render(strings.Join(lines, "\n")) + "\n\n" + styleDim.Render(m.confirmHint)
	return stylePopup.Width(w).BorderForeground(lipgloss.Color("203")).Render(body)
}

// overlay draws popup centred over the screen.
func (m *tui) overlay(screen, popup string) string {
	x := max((m.w-lipgloss.Width(popup))/2, 0)
	y := max((m.h-lipgloss.Height(popup))/2, 0)
	return lipgloss.NewCompositor(lipgloss.NewLayer(screen), lipgloss.NewLayer(popup).X(x).Y(y).Z(1)).Render()
}
