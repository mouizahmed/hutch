package main

import (
	tea "charm.land/bubbletea/v2"
)

// A second pane pins one terminal beside the workspace pane, which keeps
// following the list (or shows a view: a diff beside the agent's terminal). Only
// the focused pane gets keys; on a screen too small for two, the focused one
// fills the space and the other stays connected.

type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// minPane is the smallest a pane may be before two collapse to one.
const minPaneW, minPaneH = 40, 8

// panes are the workspace pane and, when one is pinned and there's room, the
// second; collapsed reports a pinned pane with no room to show both.
func (m *tui) panes2() (main, second rect, split, collapsed bool) {
	x, y, w, h := m.layout0()
	main = rect{x, y, w, h}
	if m.pin == "" {
		return main, rect{}, false, false
	}
	switch {
	case w >= 2*minPaneW+1:
		lw := (w-1)*m.splitPct/100 - 0
		lw = max(min(lw, w-1-minPaneW), minPaneW)
		return rect{x, y, lw, h}, rect{x + lw + 1, y, w - lw - 1, h}, true, false
	case h >= 2*minPaneH+1:
		th := max(min((h-1)*m.splitPct/100, h-1-minPaneH), minPaneH)
		return rect{x, y, w, th}, rect{x, y + th + 1, w, h - th - 1}, true, false
	}
	return main, main, false, true
}

func (m *tui) noSplit() string {
	if m.pin == "" {
		return "no second pane: v shows the selected terminal in one"
	}
	return ""
}

// toggleSplit pins the selected terminal in a second pane, or closes it.
func (m *tui) toggleSplit() tea.Cmd {
	if m.pin != "" {
		m.pin, m.focus2 = "", false
		m.resize()
		m.markHidden()
		return nil
	}
	box, n, ok := m.target(m.selected())
	if !ok {
		m.setStatus("select a running box's terminal to show it in a second pane")
		return nil
	}
	m.pin = paneKey(box, n)
	if m.panes[m.pin] == nil {
		m.connect(box, n)
	}
	m.focus2 = true
	m.setStatus(m.pin + " is in the second pane: w switches focus, v closes it")
	m.resize()
	m.markHidden()
	return nil
}

func (m *tui) swapFocus() {
	m.focus2 = !m.focus2
	m.typing = false
	m.markHidden()
}

// inputPane is the terminal keys and pastes go to: the focused pane's.
func (m *tui) inputPane() *pane {
	if m.pin != "" && m.focus2 {
		return m.panes[m.pin]
	}
	if m.view != viewNone {
		return nil
	}
	return m.shownPane()
}

// inputRect is where the focused pane is drawn.
func (m *tui) inputRect() rect {
	main, second, _, _ := m.panes2()
	if m.pin != "" && m.focus2 {
		return second
	}
	return main
}

// visible reports whether a kept terminal is on screen now.
func (m *tui) visible(key string) bool {
	_, _, split, collapsed := m.panes2()
	switch {
	case key == "":
		return false
	case key == m.pin:
		return split || collapsed && m.focus2
	case key == m.shown.Load().(string):
		return m.view == viewNone && (!collapsed || !m.focus2)
	}
	return false
}

func (m *tui) resizePanes() {
	main, second, _, collapsed := m.panes2()
	for key, p := range m.panes {
		r := main
		if key == m.pin && !collapsed {
			r = second
		}
		p.resize(r.w, r.h)
	}
}

// pinnedView draws the second pane.
func (m *tui) pinnedView(w, h int) string {
	p := m.panes[m.pin]
	if p == nil {
		return ""
	}
	if p.ended.Load() {
		why, _ := p.err.Load().(string)
		return styleDim.Render("\n  " + m.pin + ": " + why + ". v closes this pane.")
	}
	s, _ := p.render()
	return s
}
