package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/mouizahmed/hutch/internal/api"
)

// Machines shows what Hutch knows of each machine, labelled for what it is;
// its actions run the CLI's own commands.

// The Machines view: each machine's role, state, and room, and the boxes on it.

func (m *tui) openMachines() {
	m.machinev = 0
	m.setView(viewMachines)
}

func (m *tui) liveWorkers() []api.Worker {
	var ws []api.Worker
	for _, w := range m.workers {
		if !w.Gone {
			ws = append(ws, w)
		}
	}
	slices.SortFunc(ws, func(a, b api.Worker) int {
		if a.Lead != b.Lead {
			if a.Lead {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ws
}

func (m *tui) machinesView(w, h int) string {
	ws := m.liveWorkers()
	m.machinev = max(min(m.machinev, len(ws)-1), 0)
	lines := []string{styleBold.Render("Machines") + styleDim.Render(fmt.Sprintf("  %d", len(ws))), ""}
	for i, wk := range ws {
		role := "worker"
		if wk.Lead {
			role = "lead"
		}
		state := styleRead.Render("online")
		if !wk.Online {
			state = styleError.Render("offline since " + shortAge(wk.LastSeen))
		}
		awake, total := 0, 0
		for _, b := range m.boxes {
			if b.Worker == wk.ID && b.Kind != api.BoxVersion {
				total++
				if b.State == "running" {
					awake++
				}
			}
		}
		line := spread(fmt.Sprintf("  %s  %s", styleBold.Render(wk.Name), styleDim.Render(role)), state, w-1)
		if i == m.machinev {
			line = highlight(line, w-1, colorCursor)
		}
		lines = append(lines, line)
		if i == m.machinev {
			disk := "unknown"
			if wk.Disk.Total > 0 {
				disk = fmt.Sprintf("%d%% free of %.0f GB", 100*wk.Disk.Free/wk.Disk.Total, float64(wk.Disk.Total)/(1<<30))
			}
			for _, l := range []string{
				fmt.Sprintf("hutchd %s · %s", wk.Version, wk.Class),
				fmt.Sprintf("%d CPUs · %s memory: %s reserved by awake boxes, %s in use at its last report", wk.CPUs, gb(wk.MemMiB), gb(wk.ReservedMiB), gb(wk.UsedMiB)),
				"disk " + disk,
				fmt.Sprintf("%d box(es), %d awake", total, awake),
			} {
				lines = append(lines, styleDim.Render("      "+truncate(l, w-8)))
			}
			if wk.SSH != "" {
				lines = append(lines, styleDim.Render("      ssh "+wk.SSH))
			}
		}
	}
	lines = append(lines, "", styleDim.Render(truncate("a add · r remove (moves its boxes) · L remove as lost · l make lead · u upgrade all · D doctor · esc", w-1)))
	return scrollLines(lines, new(int), w, h)
}

func (m *tui) machinesKey(msg tea.KeyPressMsg) tea.Cmd {
	ws := m.liveWorkers()
	var wk api.Worker
	if m.machinev < len(ws) {
		wk = ws[m.machinev]
	}
	switch msg.String() {
	case "esc", "q", "m":
		m.setView(viewNone)
	case "j", "down":
		m.machinev = min(m.machinev+1, len(ws)-1)
	case "k", "up":
		m.machinev = max(m.machinev-1, 0)
	case "a":
		return m.addMachine()
	case "u":
		return m.interactive(m.cwd, "machine", "upgrade")
	case "D":
		return m.interactive(m.cwd, "doctor")
	case "r", "L", "l":
		if wk.ID == "" {
			return nil
		}
		if wk.Lead && msg.String() != "l" {
			m.setStatus(wk.Name + " leads: hand the lead to another machine first (l on it)")
			return nil
		}
		switch msg.String() {
		case "r":
			m.ask("Remove "+wk.Name+"?\n\nIts boxes move to the other machines first; one that can't stops the removal.", "y removes it · any other key keeps it", func() tea.Cmd {
				return m.interactive(m.cwd, "machine", "rm", wk.Name)
			})
		case "L":
			m.ask("Remove "+wk.Name+" as lost?\n\nIt's fenced over SSH first; its boxes come back elsewhere from their last snapshots, and work since then is lost.", "y goes on · any other key doesn't", func() tea.Cmd {
				return m.interactive(m.cwd, "machine", "rm", "--lost", wk.Name)
			})
		case "l":
			if wk.Lead {
				m.setStatus(wk.Name + " already leads")
				return nil
			}
			m.ask("Make "+wk.Name+" the lead?\n\nChanges pause for seconds while the lead hands over; boxes keep running.", "y hands over · any other key doesn't", func() tea.Cmd {
				return m.interactive(m.cwd, "machine", "lead", wk.Name)
			})
		}
	}
	return nil
}

// Changes to a box or terminal, through the CLI's own commands.

func (m *tui) moveBox(b api.Box) tea.Cmd {
	items := []menuItem{{label: "Auto", detail: "the scheduler's choice", do: func() tea.Cmd { return m.confirmMove(b, "") }}}
	for _, wk := range m.liveWorkers() {
		why := ""
		switch {
		case wk.ID == b.Worker:
			why = b.ID + " is already there"
		case !wk.Online:
			why = wk.Name + " is offline"
		}
		items = append(items, menuItem{label: wk.Name, detail: wk.Class, why: why, do: func() tea.Cmd { return m.confirmMove(b, wk.Name) }})
	}
	m.choose("Move "+b.ID+" to", items)
	return nil
}

func (m *tui) confirmMove(b api.Box, to string) tea.Cmd {
	where := "the machine the scheduler picks"
	args := []string{"move", b.ID}
	if to != "" {
		where, args = to, append(args, "--to", to)
	}
	text := "Move " + b.ID + " to " + where + "?\n\nWithin one snapshot class it moves warm (processes keep running); across classes it boots cold: files kept, processes restart. The result says which."
	return m.confirmRun(text, m.cwd, "move "+b.ID, args...)
}

func (m *tui) resizeBox(b api.Box) tea.Cmd {
	m.form2("Resize "+b.ID+": a cold boot, so its processes restart; disk only grows",
		"tab next field · enter resize · esc", []string{"CPUs", "Memory", "Disk"},
		[]string{"vCPUs", "like 16G", "like 100G (only grows)"},
		[]string{strconv.Itoa(b.VCPUs), sizeValue(b.MemMiB), ""},
		func(v []string) (tea.Cmd, string) {
			args := []string{"resize", b.ID}
			if c := strings.TrimSpace(v[0]); c != "" && c != strconv.Itoa(b.VCPUs) {
				if n, err := strconv.Atoi(c); err != nil || n < 1 {
					return nil, "CPUs is a whole number"
				}
				args = append(args, "--cpu", c)
			}
			if mem := strings.TrimSpace(v[1]); mem != "" && mem != sizeValue(b.MemMiB) {
				args = append(args, "--memory", mem)
			}
			if d := strings.TrimSpace(v[2]); d != "" {
				args = append(args, "--disk", d)
			}
			if len(args) == 2 {
				return nil, "nothing changed"
			}
			return m.confirmRun("Resize "+b.ID+": "+strings.Join(args[2:], " ")+"?\n\nIt sleeps, takes the new size, and boots cold; processes restart, and it may move to another machine if it no longer fits.",
				m.cwd, "resize "+b.ID, args...), ""
		})
	return nil
}

func (m *tui) renameTerminal(r tuiRow) tea.Cmd {
	key := r.key()
	m.input("Rename terminal "+key+" (now: "+r.thread.Title+"); empty gives the title back to its program", "a name", "", func(name string) tea.Cmd {
		t := m.run(m.cwd, "rename "+key, key, "terminal", "rename", key, name)
		return t
	})
	return nil
}

// gb is MiB as GB, to one decimal.
func gb(miB int) string { return fmt.Sprintf("%.1f GB", float64(miB)/1024) }
