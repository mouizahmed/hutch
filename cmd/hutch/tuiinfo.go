package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mouizahmed/hutch/internal/agents"
	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Details, Machines, and Settings show what Hutch knows, labelled for what it
// is (an allocation, a last report, unknown); looking never wakes a box. Their
// actions run the CLI's own commands.

type infoView struct {
	box    api.Box
	live   *guestproto.Status // a running box's own report
	err    string
	scroll int
}

type infoMsg struct {
	box    string
	status guestproto.Status
	err    error
}

func (m *tui) openInfo(b api.Box) tea.Cmd {
	m.infov = &infoView{box: b}
	m.setView(viewInfo)
	if b.State != "running" {
		return nil
	}
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var st guestproto.Status
		err := rpc.Call(ctx, sess, api.BoxStatus, api.IDParams{ID: b.ID}, &st)
		return infoMsg{box: b.ID, status: st, err: err}
	}
}

// current is the box as the latest events have it, for a view opened on it.
func (m *tui) current(id string) (api.Box, bool) {
	i := slices.IndexFunc(m.boxes, func(b api.Box) bool { return b.ID == id })
	if i < 0 {
		return api.Box{}, false
	}
	return m.boxes[i], true
}

func (m *tui) infoLines() []string {
	v := m.infov
	b := v.box
	if cur, ok := m.current(b.ID); ok {
		b = cur
	}
	field := func(k, val string) string {
		if val == "" {
			val = styleDim.Render("—")
		}
		return fmt.Sprintf("  %-14s %s", k, val)
	}
	kind := "box"
	switch b.Kind {
	case api.BoxTemplate:
		kind = "template box"
	case api.BoxVersion:
		kind = "template version (read-only)"
	}
	state := b.State
	if b.Machine != "" && b.State != "running" {
		state += " on " + b.Machine
	}
	snap := "none yet"
	switch {
	case b.Generation > 0 && b.Uploaded >= b.Generation:
		snap = fmt.Sprintf("generation %d, in the bucket", b.Generation)
	case b.Generation > 0 && b.Uploaded > 0:
		snap = fmt.Sprintf("generation %d; the bucket has %d", b.Generation, b.Uploaded)
	case b.Generation > 0:
		snap = fmt.Sprintf("generation %d, not in the bucket yet", b.Generation)
	}
	machine := b.Machine
	if machine == "" {
		machine = b.Worker
	}
	lines := []string{styleBold.Render("Details") + styleDim.Render("  "+b.ID), "",
		field("Project", m.projectName(b.Project)),
		field("Kind", kind),
		field("State", state),
		field("Machine", machine),
		field("Branch", b.Branch),
		field("Base", b.Base),
		field("Allocation", fmt.Sprintf("%d vCPU, %s memory (what it's given, not what it uses)", b.VCPUs, mib(b.MemMiB))),
		field("Image", b.Image),
		field("Snapshot", snap),
		field("Snapshot class", b.Class),
		field("Created", b.Created.Local().Format(time.DateTime)+" ("+ago(b.Created)+")"),
		field("Stable ID", b.UID),
		field("Ownership", b.Owner),
		field("Checkout", b.Path),
	}
	work := "nothing only in the box"
	if b.Changes > 0 || b.Unpushed > 0 {
		work = fmt.Sprintf("%d uncommitted file(s), %d unpushed commit(s), as of its last report", b.Changes, b.Unpushed)
	}
	lines = append(lines, field("Work", work), "", styleLabel.Render("Terminals"))
	for _, t := range b.Threads {
		lines = append(lines, fmt.Sprintf("  %d  %-10s %-8s %s", t.N, t.Agent, t.State, t.Title))
	}
	if len(b.Threads) == 0 {
		lines = append(lines, styleDim.Render("  none"))
	}
	lines = append(lines, "", styleLabel.Render("Live"))
	switch {
	case b.State != "running":
		lines = append(lines, styleDim.Render("  asleep: not asked (that would wake it)"))
	case v.err != "":
		lines = append(lines, styleError.Render("  "+v.err))
	case v.live == nil:
		lines = append(lines, styleDim.Render("  asking the box…"))
	default:
		var ports []string
		for _, p := range v.live.Ports {
			ports = append(ports, strconv.Itoa(p.Port))
		}
		lines = append(lines, field("Listening", strings.Join(ports, ", ")),
			field("Attached", fmt.Sprintf("%d terminal client(s), %d keeping it awake, %d other session(s)", v.live.Clients, v.live.Viewers, v.live.Sessions)))
	}
	return lines
}

func (m *tui) infoView(w, h int) string {
	lines := append(m.infoLines(), "", styleDim.Render(truncate("c console · D doctor · y copy these details · g git · o ports · esc", w-1)))
	return scrollLines(lines, &m.infov.scroll, w, h)
}

func (m *tui) infoKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.infov
	switch msg.String() {
	case "esc", "q":
		m.setView(viewNone)
	case "j", "down":
		v.scroll++
	case "k", "up":
		v.scroll = max(v.scroll-1, 0)
	case "c":
		m.ask("Open "+v.box.ID+"'s serial console?\n\nIt's the box's own console, for when its guest is broken; Enter ~ . quits.", "y opens it · any other key doesn't", func() tea.Cmd {
			return m.interactive(m.cwd, "console", v.box.ID)
		})
	case "D":
		return m.run(m.cwd, "doctor", "", "doctor")
	case "y":
		var plain []string
		for _, l := range m.infoLines() {
			plain = append(plain, strings.TrimRight(ansi.Strip(l), " "))
		}
		m.setStatus("copied " + v.box.ID + "'s details")
		return tea.SetClipboard(strings.Join(plain, "\n"))
	case "g":
		return m.openGit(v.box)
	case "o":
		return m.openPorts(v.box)
	}
	return nil
}

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
		return m.run(m.cwd, "doctor", "", "doctor")
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

// The Settings view: the effective config with where each value comes from,
// the logins captured, and grants. Secret values aren't shown; a captured login
// isn't proof the provider still takes it.

type settingsView struct {
	cfg     *config.Config
	user    *api.UserState
	err     string
	scroll  int
	loading bool
}

type settingsMsg struct {
	cfg  *config.Config
	user *api.UserState
	err  error
}

func (m *tui) openSettings() tea.Cmd {
	m.setv = &settingsView{loading: true}
	m.setView(viewSettings)
	return m.reloadSettings()
}

func (m *tui) reloadSettings() tea.Cmd {
	if m.setv == nil {
		return nil
	}
	ctx, sess := m.ctx, m.sess
	return func() tea.Msg {
		cfg, err := loadConfig(ctx, sess)
		if err != nil {
			return settingsMsg{err: err}
		}
		var st api.UserState
		err = rpc.Call(ctx, sess, api.UserGet, struct{}{}, &st)
		return settingsMsg{cfg: cfg, user: &st, err: err}
	}
}

// secretKey is a config key whose value isn't shown.
func secretKey(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, "token") || strings.HasSuffix(p, "secret") || strings.HasSuffix(p, "password")
}

func (m *tui) settingsView(w, h int) string {
	v := m.setv
	lines := []string{styleBold.Render("Settings"), ""}
	switch {
	case v.err != "":
		lines = append(lines, styleError.Render(v.err))
	case v.cfg == nil:
		lines = append(lines, styleDim.Render("reading…"))
	}
	if v.user != nil {
		lines = append(lines, styleLabel.Render("Logins"))
		set, err := agents.Load(v.user.Layer.Adapters)
		if err != nil {
			lines = append(lines, styleError.Render("  "+err.Error()))
		}
		logins := map[string]api.Credentials{}
		for _, c := range v.user.Credentials {
			logins[c.Part] = c
		}
		for _, a := range set.Sorted() {
			line := fmt.Sprintf("  %-8s config: %d files", a.Name, len(v.user.Layer.Files[a.Name]))
			if a.Login != "" {
				if state := loginState(a, logins); state == "" {
					line += fmt.Sprintf(", login captured %s (not proof the provider still takes it)", ago(logins[a.Name].Updated))
				} else {
					line += ", " + state
				}
			}
			if _, ok := logins[a.Name+"/mcp"]; ok {
				line += ", MCP secrets"
			}
			lines = append(lines, truncate(line, w-1))
		}
		if v.user.Version == 0 {
			lines = append(lines, styleDim.Render("  nothing synced yet: palette → user sync"))
		}
		lines = append(lines, "")
	}
	if v.cfg != nil {
		lines = append(lines, styleLabel.Render("Effective config")+styleDim.Render("  (value · where it's set)"))
		for _, s := range v.cfg.All() {
			val := s.String()
			if secretKey(s.Path) {
				val = "(hidden)"
			}
			lines = append(lines, truncate(fmt.Sprintf("  %-28s %s", s.Path, val), w-24)+styleDim.Render("  "+truncate(from(s), 22)))
		}
	}
	lines = append(lines, "", styleDim.Render(truncate("e edit your config · M machine config · P project settings · L log in · s sync · G grants · r reload · esc", w-1)))
	return scrollLines(lines, &v.scroll, w, h)
}

func (m *tui) settingsKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.setv
	_, dir := m.newProject()
	switch msg.String() {
	case "esc", "q", ",":
		m.setView(viewNone)
	case "j", "down":
		v.scroll++
	case "k", "up":
		v.scroll = max(v.scroll-1, 0)
	case "r":
		return m.reloadSettings()
	case "e":
		return m.interactive(m.cwd, "config", "edit")
	case "M":
		return m.interactive(m.cwd, "config", "edit", "--machine")
	case "P":
		if dir == "" {
			m.setStatus("open a project's tab with a checkout here first")
			return nil
		}
		return m.interactive(dir, "config", "edit", "--project")
	case "L":
		return m.loginMenu()
	case "s":
		return m.interactive(m.cwd, "user", "sync")
	case "G":
		return m.run(m.cwd, "grants", "", "config", "grants")
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

func (m *tui) sendToTerminal(r tuiRow) tea.Cmd {
	key := r.key()
	m.input("Type a message into "+key+" ("+r.thread.Title+") and press Enter there", "the message", "", func(text string) tea.Cmd {
		if text == "" {
			return nil
		}
		return m.run(m.cwd, "send to "+key, key, "send", key, text)
	})
	return nil
}

// gb is MiB as GB, to one decimal.
func gb(miB int) string { return fmt.Sprintf("%.1f GB", float64(miB)/1024) }
