package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/hashicorp/yamux"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/boxgit"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// The Git view reviews a box's /workspace as dev: the same branch, index, and
// worktree the agent uses. Looking changes nothing and contacts no remote;
// staging, committing, and talking to the remote are each a deliberate key.

// workspace is a box's checkout.
const workspace = "/workspace"

type gitView struct {
	box     api.Box // captured when opened: the view never changes target
	status  boxgit.Status
	loaded  bool
	err     string
	pos     int // the selected file, in listed order
	diff    *boxgit.Diff
	diffFor boxgit.File
	branch  bool // showing the task diff against the base, not one file
	scroll  int
	hunk    int
	busy    string
	at      time.Time // when status was read
	want    int       // the diff read last asked for; an earlier one's reply is dropped
	asked   int       // likewise for the status
	loading bool
}

type (
	gitStatusMsg struct {
		box    string
		asked  int
		status boxgit.Status
		err    error
	}
	gitDiffMsg struct {
		box    string
		want   int
		file   boxgit.File
		branch bool
		diff   boxgit.Diff
		err    error
	}
	gitDoneMsg struct {
		box, did string
		err      error
	}
)

// boxRun runs commands in a box's /workspace over the machine's exec.
func boxRun(sess *yamux.Session, box string) boxgit.Run {
	return func(ctx context.Context, argv, env []string) ([]byte, []byte, int, error) {
		var out, errOut bytes.Buffer
		code, err := rpc.Exec(ctx, sess, api.BoxExec, api.ExecParams{ID: box, Dir: workspace, Argv: argv, Env: env}, &out, &errOut)
		return out.Bytes(), errOut.Bytes(), code, err
	}
}

func (m *tui) openGit(b api.Box) tea.Cmd {
	if b.State != "running" {
		m.ask(b.ID+" is "+b.State+". The Git view reads its checkout live, so it has to be awake.\n\nWake it?", "y wakes it · any other key doesn't", func() tea.Cmd {
			return tea.Batch(m.sleepWake(b), func() tea.Msg { return doneMsg{status: "g opens Git once " + b.ID + " is awake"} })
		})
		return nil
	}
	m.gitv = &gitView{box: b}
	m.setView(viewGit)
	return m.gitRefresh()
}

func (m *tui) gitRefresh() tea.Cmd {
	v := m.gitv
	m.gitN++
	v.asked = m.gitN
	ctx, run, box, asked := m.ctx, boxRun(m.sess, v.box.ID), v.box.ID, v.asked
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		s, err := boxgit.GetStatus(ctx, run)
		return gitStatusMsg{box: box, asked: asked, status: s, err: err}
	}
}

func (m *tui) gitLoadDiff(f boxgit.File, branch bool) tea.Cmd {
	v := m.gitv
	m.gitN++
	v.want, v.loading = m.gitN, true
	ctx, run, box, limit, base, want := m.ctx, boxRun(m.sess, v.box.ID), v.box.ID, m.diffLimit, v.box.Base, v.want
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var d boxgit.Diff
		var err error
		if branch {
			d, err = boxgit.BranchDiff(ctx, run, base, limit)
		} else {
			d, err = boxgit.GetDiff(ctx, run, f, limit)
		}
		return gitDiffMsg{box: box, want: want, file: f, branch: branch, diff: d, err: err}
	}
}

// gitDo runs a change to the repository, then reads it again.
func (m *tui) gitDo(did string, f func(ctx context.Context, run boxgit.Run) error) tea.Cmd {
	return m.gitDoSay(did, func(ctx context.Context, run boxgit.Run) (string, error) { return did, f(ctx, run) })
}

// gitDoSay is gitDo where the change says how it went.
func (m *tui) gitDoSay(doing string, f func(ctx context.Context, run boxgit.Run) (string, error)) tea.Cmd {
	v := m.gitv
	v.busy = doing
	ctx, run, box := m.ctx, boxRun(m.sess, v.box.ID), v.box.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		did, err := f(ctx, run)
		if err != nil {
			did = doing
		}
		return gitDoneMsg{box: box, did: did, err: err}
	}
}

func (m *tui) gitMsg(msg tea.Msg) tea.Cmd {
	v := m.gitv
	if v == nil {
		return nil
	}
	switch msg := msg.(type) {
	case gitStatusMsg:
		if msg.box != v.box.ID || msg.asked != v.asked {
			return nil
		}
		v.loaded, v.at = true, time.Now()
		v.err = ""
		if msg.err != nil {
			v.err = msg.err.Error()
			return nil
		}
		v.status = msg.status
		v.pos = max(min(v.pos, len(v.status.Files)-1), 0)
		// The open diff is read again, as it may have moved on, and the cursor
		// stays on its file as the groups change.
		if v.diff != nil && !v.branch {
			for at, i := range v.ordered() {
				if f := v.status.Files[i]; f.Path == v.diffFor.Path && f.Kind == v.diffFor.Kind {
					v.pos = at
					return m.gitLoadDiff(f, false)
				}
			}
			v.diff = nil
		}
	case gitDiffMsg:
		if msg.box != v.box.ID || msg.want != v.want {
			return nil
		}
		v.loading = false
		if msg.err != nil {
			v.err = msg.err.Error()
			return nil
		}
		d := msg.diff
		if v.diff == nil || v.diffFor != msg.file || v.branch != msg.branch {
			v.scroll, v.hunk = 0, 0
		}
		v.diff, v.diffFor, v.branch = &d, msg.file, msg.branch
		v.hunk = max(min(v.hunk, len(d.Hunks)-1), 0)
	case gitDoneMsg:
		if msg.box != v.box.ID {
			return nil
		}
		v.busy = ""
		if msg.err != nil {
			m.setStatus(msg.did + ": " + msg.err.Error())
		} else {
			m.setStatus(msg.did)
		}
		return m.gitRefresh()
	}
	return nil
}

func (m *tui) gitKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.gitv
	f, ok := v.selected()
	b := v.box
	inDiff := v.diff != nil
	if inDiff {
		f, ok = v.diffFor, !v.branch // keys act on the file shown, and the task diff is none
	}
	_, _, _, h := m.layout()
	switch msg.String() {
	case "esc", "q", "h", "left":
		if inDiff || v.loading {
			m.gitN++ // a diff still being read stays closed
			v.diff, v.branch, v.loading, v.want = nil, false, false, m.gitN
			return nil
		}
		m.setView(viewNone)
		return nil
	case "r":
		return m.gitRefresh()
	case "j", "down":
		if inDiff {
			v.scroll++
		} else {
			v.pos = min(v.pos+1, len(v.status.Files)-1)
		}
	case "k", "up":
		if inDiff {
			v.scroll = max(v.scroll-1, 0)
		} else {
			v.pos = max(v.pos-1, 0)
		}
	case "pgdown", "ctrl+d":
		v.scroll += max(h-6, 1)
	case "pgup", "ctrl+u":
		v.scroll = max(v.scroll-max(h-6, 1), 0)
	case "enter", "l", "right":
		if !inDiff && ok {
			return m.gitLoadDiff(f, false)
		}
	case "tab":
		if b.Base == "" {
			m.setStatus(b.ID + " has no base branch to compare with")
			return nil
		}
		return m.gitLoadDiff(boxgit.File{}, true)
	case "]", "n":
		if inDiff && len(v.diff.Hunks) > 0 {
			v.hunk = min(v.hunk+1, len(v.diff.Hunks)-1)
			v.scroll = v.diff.Hunks[v.hunk].Start
		}
	case "[", "N":
		if inDiff && len(v.diff.Hunks) > 0 {
			v.hunk = max(v.hunk-1, 0)
			v.scroll = v.diff.Hunks[v.hunk].Start
		}
	case "a":
		if !ok || f.Kind == boxgit.Staged {
			return nil
		}
		return m.gitDo("staged "+f.Path, func(ctx context.Context, run boxgit.Run) error { return boxgit.StageFile(ctx, run, f) })
	case "u":
		if !ok || f.Kind != boxgit.Staged {
			return nil
		}
		unborn := v.status.Unborn
		return m.gitDo("unstaged "+f.Path, func(ctx context.Context, run boxgit.Run) error { return boxgit.UnstageFile(ctx, run, f, unborn) })
	case "A", "U":
		if !inDiff || v.branch || len(v.diff.Hunks) == 0 {
			m.setStatus("open a file's diff, then [ ] pick a hunk")
			return nil
		}
		want := boxgit.Unstaged
		verb := "staged"
		if msg.String() == "U" {
			want, verb = boxgit.Staged, "unstaged"
		}
		if v.diffFor.Kind != want {
			m.setStatus(fmt.Sprintf("this is the %s change: %s works on the %s one", v.diffFor.Kind, msg.String(), want))
			return nil
		}
		file, rev, i := v.diffFor, v.diff.Revision, v.hunk
		return m.gitDo(fmt.Sprintf("%s hunk %d of %s", verb, i+1, file.Path), func(ctx context.Context, run boxgit.Run) error {
			return boxgit.StageHunk(ctx, run, file, rev, i)
		})
	case "c":
		if !v.status.Staged() {
			m.setStatus("nothing staged to commit: a stages a file, A a hunk")
			return nil
		}
		index := v.status.Index
		m.writeMessage("Commit the staged changes in "+b.ID, "The commit message", func(text string) (tea.Cmd, string) {
			if strings.TrimSpace(text) == "" {
				return nil, "write a commit message"
			}
			return m.gitDoSay("committing", func(ctx context.Context, run boxgit.Run) (string, error) {
				line, err := boxgit.Commit(ctx, run, text, index)
				return "committed " + line, err
			}), ""
		})
	case "f":
		return m.gitTask("fetch", "exec", b.ID, "--", "git", "-C", workspace, "fetch")
	case "p":
		switch {
		case v.status.Branch == "":
			m.setStatus("HEAD is detached in " + b.ID + ": check out a branch in a terminal first")
			return nil
		case v.status.Upstream == "":
			m.setStatus(v.status.Branch + " in " + b.ID + " has no upstream to pull from: P pushes it and sets one")
			return nil
		}
		return m.gitRemote("pull", "y pulls", "Pull "+v.status.Upstream+" into "+v.status.Branch+" in "+b.ID+", fast-forward only?\n\nIf the branches diverged it stops and says so; nothing is merged or rebased.",
			"exec", b.ID, "--", "git", "-C", workspace, "pull", "--ff-only")
	case "P":
		if v.status.Branch == "" {
			m.setStatus("HEAD is detached in " + b.ID + ": check out a branch in a terminal first")
			return nil
		}
		if v.status.Upstream == "" {
			return m.gitRemote("push", "y pushes", "Push "+v.status.Branch+" from "+b.ID+" to origin, and track it there?\n\nIt has no upstream yet.",
				"exec", b.ID, "--", "git", "-C", workspace, "push", "--set-upstream", "origin", "HEAD")
		}
		return m.gitRemote("push", "y pushes", fmt.Sprintf("Push %s from %s to %s?\n\n%d commit(s) ahead, %d behind (as of the last fetch). Nothing is forced.",
			v.status.Branch, b.ID, v.status.Upstream, v.status.Ahead, v.status.Behind),
			"exec", b.ID, "--", "git", "-C", workspace, "push")
	}
	return nil
}

func (m *tui) gitRemote(verb, yes, question string, args ...string) tea.Cmd {
	m.ask(question, yes+" · any other key doesn't", func() tea.Cmd { return m.gitTask(verb, args...) })
	return nil
}

// gitTask runs a git command that talks to the remote as a task, and reads the
// status again when it ends well.
func (m *tui) gitTask(verb string, args ...string) tea.Cmd {
	box := m.gitv.box.ID
	cmd := m.run(m.cwd, verb+" in "+box, "", args...)
	m.tasks[len(m.tasks)-1].after = func() tea.Msg { return gitRefreshMsg{box} }
	return cmd
}

type gitRefreshMsg struct{ box string }

func (m *tui) gitView(w, h int) string {
	v := m.gitv
	b := v.box
	lines := []string{styleBold.Render("Git") + styleDim.Render("  "+b.ID+" · /workspace")}
	s := v.status
	if v.loaded && v.err == "" {
		branch := s.Branch
		if branch == "" {
			branch = "detached HEAD"
		}
		if s.Unborn {
			branch += " (no commits yet)"
		}
		up := " → no upstream yet"
		if s.Upstream != "" {
			up = " → " + s.Upstream
			switch {
			case s.Ahead == 0 && s.Behind == 0:
				up += " · in step as of the last fetch"
			default:
				var ab []string
				if s.Ahead > 0 {
					ab = append(ab, fmt.Sprintf("%d ahead", s.Ahead))
				}
				if s.Behind > 0 {
					ab = append(ab, fmt.Sprintf("%d behind", s.Behind))
				}
				up += " · " + strings.Join(ab, ", ") + " as of the last fetch"
			}
		}
		lines = append(lines, truncate(styleWorking.Render(branch)+styleDim.Render(up), w-1))
	}
	switch {
	case v.err != "":
		lines = append(lines, "", styleError.Render(truncate(v.err, w-1)), "", styleDim.Render("r tries again · esc back"))
		return strings.Join(lines, "\n")
	case !v.loaded:
		return strings.Join(append(lines, "", styleDim.Render("reading the repository…")), "\n")
	}
	if v.diff != nil {
		return strings.Join(append(lines, m.gitDiffView(w, h-len(lines))...), "\n")
	}
	lines = append(lines, workSummary(s))
	hd := len(lines)
	if v.busy != "" {
		lines = append(lines, styleDim.Render(v.busy+"…"))
	}
	selLine := 0
	kind := boxgit.Kind(-1)
	ordered := v.ordered()
	for pos, i := range ordered {
		f := s.Files[i]
		if f.Kind != kind {
			kind = f.Kind
			n := 0
			for _, g := range s.Files {
				if g.Kind == kind {
					n++
				}
			}
			title := strings.ToUpper(kind.String()[:1]) + kind.String()[1:]
			lines = append(lines, "", styleLabel.Render(title)+styleDim.Render(fmt.Sprintf(" %d", n)))
		}
		name := f.Path
		if f.Orig != "" {
			name = f.Orig + " → " + f.Path
		}
		right := lineCounts(f.Added, f.Removed)
		if f.Binary {
			right = styleDim.Render("binary")
		}
		line := spread(fmt.Sprintf("  %s %s", gitCode(f), name), right, w-1)
		if pos == v.pos {
			line, selLine = highlight(line, w-1, colorCursor), len(lines)
		}
		lines = append(lines, line)
	}
	if len(ordered) == 0 {
		msg := "  Clean: nothing uncommitted, and nothing waiting to be pushed."
		if len(s.Commits) > 0 {
			msg = "  Nothing uncommitted."
		}
		lines = append(lines, "", styleDim.Render(msg))
	}
	if len(s.Commits) > 0 {
		lines = append(lines, "", spread(styleLabel.Render("Not pushed")+styleDim.Render(fmt.Sprintf(" %d", len(s.Commits))), styleDim.Render("tab shows the task diff"), w-1))
		for _, c := range s.Commits {
			lines = append(lines, truncate("  "+styleUnread.Render(c.Hash)+" "+c.Subject, w-1))
		}
	}
	lines = append(lines, "", styleDim.Render(truncate("enter diff · a/u stage/unstage · c commit · tab task diff · f fetch · p pull · P push · r refresh · esc back", w-1)))
	// Keep the selected file on screen, under the header.
	if len(lines) > h {
		cut := min(len(lines)-h, max(selLine-(h-4), 0))
		lines = append(lines[:hd], lines[hd+cut:]...)
		if len(lines) > h {
			lines = append(lines[:h-1], lines[len(lines)-1])
		}
	}
	return strings.Join(lines, "\n")
}

// workSummary is one line on the box's work: files and lines not yet committed,
// and commits not yet pushed.
func workSummary(s boxgit.Status) string {
	files := map[string]bool{}
	added, removed := 0, 0
	for _, f := range s.Files {
		files[f.Path] = true
		added += f.Added
		removed += f.Removed
	}
	var parts []string
	if len(files) > 0 {
		parts = append(parts, styleDim.Render(plural(fmt.Sprint(len(files)), "file")+" changed")+" "+lineCounts(added, removed))
	}
	if n := len(s.Commits); n > 0 {
		more := ""
		if n == boxgit.MaxCommits {
			more = "+"
		}
		parts = append(parts, styleWorking.Render(plural(fmt.Sprint(n)+more, "commit")+" not pushed"))
	}
	if len(parts) == 0 {
		return styleDim.Render("no changes")
	}
	return strings.Join(parts, styleDim.Render(" · "))
}

// gitCode is a file's status letter, colored by what it does.
func gitCode(f boxgit.File) string {
	c := string(f.Code)
	switch f.Code {
	case 'A', '?':
		return styleRead.Render(c)
	case 'D':
		return styleError.Render(c)
	case 'U':
		return styleError.Render("U")
	}
	return styleWorking.Render(c)
}

// gitMeta reports whether a diff line is Git's bookkeeping rather than content.
func gitMeta(l string) bool {
	for _, p := range []string{"index ", "--- ", "+++ ", "new file mode", "deleted file mode", "old mode", "new mode", "similarity index", "rename from", "rename to", "Binary files"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// selected is the file under the cursor.
func (v *gitView) selected() (boxgit.File, bool) {
	o := v.ordered()
	if v.pos < 0 || v.pos >= len(o) {
		return boxgit.File{}, false
	}
	return v.status.Files[o[v.pos]], true
}

// files are listed grouped, so the cursor walks them in that order.
func (v *gitView) ordered() []int {
	var out []int
	for _, k := range []boxgit.Kind{boxgit.Conflict, boxgit.Staged, boxgit.Unstaged, boxgit.Untracked} {
		for i, f := range v.status.Files {
			if f.Kind == k {
				out = append(out, i)
			}
		}
	}
	return out
}

func (m *tui) gitDiffView(w, h int) []string {
	v := m.gitv
	d := v.diff
	lines := d.Lines()
	added, removed := 0, 0
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			added++
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			removed++
		}
	}
	title := styleBold.Render("Task diff") + styleDim.Render(" since origin/"+v.box.Base+", committed and not")
	if !v.branch {
		title = styleBold.Render(v.diffFor.Path) + styleDim.Render(" · "+v.diffFor.Kind.String())
		if n := len(d.Hunks); n > 0 {
			title += styleDim.Render(fmt.Sprintf(" · hunk %d of %d", v.hunk+1, n))
		}
	}
	out := []string{spread(title, lineCounts(added, removed), w-1)}
	if d.Cut {
		out = append(out, styleError.Render("Longer than tui.diff_limit: the rest isn't shown, and hunks past the cut can't be staged here."))
	}
	rows := max(h-len(out)-2, 1)
	v.scroll = max(min(v.scroll, len(lines)-rows), 0)
	var cur boxgit.Hunk
	if len(d.Hunks) > 0 && !v.branch {
		cur = d.Hunks[v.hunk]
	}
	for i := v.scroll; i < len(lines) && len(out) < rows+1; i++ {
		// A tab is one rune but several columns: expand it, so the line fits.
		l := truncate(fmt.Sprintf("%4d %s", i+1, strings.ReplaceAll(lines[i], "\t", "    ")), w-1)
		switch {
		case strings.HasPrefix(lines[i], "+") && !strings.HasPrefix(lines[i], "+++"):
			l = styleRead.Render(l)
		case strings.HasPrefix(lines[i], "-") && !strings.HasPrefix(lines[i], "---"):
			l = styleError.Render(l)
		case strings.HasPrefix(lines[i], "@@"):
			l = styleUnread.Render(l)
		case strings.HasPrefix(lines[i], "diff --git "):
			// A file's start in the task diff: its name, set off.
			name := lines[i][strings.LastIndex(lines[i], " b/")+3:]
			l = styleLabel.Render(truncate(fmt.Sprintf("%4s ── %s ", "", name), w-1))
		case gitMeta(lines[i]):
			l = styleDim.Render(l)
		}
		if i >= cur.Start && i < cur.End && cur.End > 0 {
			l = "▌" + l
		} else {
			l = " " + l
		}
		out = append(out, l)
	}
	if len(lines) == 0 {
		out = append(out, styleDim.Render("  (no textual change)"))
	}
	keys := "j/k pgup/pgdn scroll · [ ] hunks · A/U stage/unstage hunk · esc files"
	if v.branch {
		keys = "j/k pgup/pgdn scroll · esc files"
	}
	return append(out, "", styleDim.Render(truncate(keys, w-1)))
}
