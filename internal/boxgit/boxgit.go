// Package boxgit is the Git review a client does in a box's checkout: status,
// diffs, staging whole files or single hunks, and commits. Every operation is
// native Git run in the repository through Run, with literal, NUL-separated
// paths; reads take no optional locks and run no external diff or textconv
// programs, so looking changes nothing.
package boxgit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Run runs argv in the repository with env added, and returns its standard
// output, standard error, and exit code.
type Run func(ctx context.Context, argv, env []string) (stdout, stderr []byte, code int, err error)

// readEnv keeps reads from writing the index (refreshing its stat data) and
// from running anything the repository configures.
var readEnv = []string{"GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "GIT_PAGER=cat", "LC_ALL=C"}

// literal makes every path a name, never a pattern: a file called *.go is that
// file alone.
const literal = "GIT_LITERAL_PATHSPECS=1"

// Kind is which change a diff shows.
type Kind int

const (
	Unstaged  Kind = iota // the index against the worktree
	Staged                // HEAD against the index
	Untracked             // a new file's contents
	Conflict              // an unmerged path, as Git shows it
)

func (k Kind) String() string {
	return [...]string{"unstaged", "staged", "untracked", "conflicted"}[k]
}

// File is one changed path. A file staged and edited again appears twice, once
// per Kind.
type File struct {
	Path string
	Orig string // a rename or copy's source
	Kind Kind
	Code byte // Git's status letter: M, A, D, R, C, T, U, ?
}

// Status is the repository as it is now.
type Status struct {
	Branch   string // empty when detached
	Unborn   bool   // no commit yet
	Upstream string
	Ahead    int
	Behind   int
	Files    []File
	Index    string // changes whenever what's staged does
}

// Staged reports whether anything is staged to commit.
func (s Status) Staged() bool {
	for _, f := range s.Files {
		if f.Kind == Staged {
			return true
		}
	}
	return false
}

func git(ctx context.Context, run Run, env []string, args ...string) ([]byte, error) {
	out, errOut, code, err := run(ctx, append([]string{"git"}, args...), append([]string{literal}, env...))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return out, fmt.Errorf("git %s: %s", args[0], firstLine(msg, fmt.Sprintf("exited %d", code)))
	}
	return out, nil
}

func firstLine(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// GetStatus reads the branch, its upstream, and every changed path.
func GetStatus(ctx context.Context, run Run) (Status, error) {
	out, err := git(ctx, run, readEnv, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(out)
}

func parseStatus(out []byte) (Status, error) {
	var s Status
	index := sha256.New()
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		e := fields[i]
		if e == "" {
			continue
		}
		switch {
		case strings.HasPrefix(e, "# branch.oid "):
			s.Unborn = strings.HasSuffix(e, "(initial)")
		case strings.HasPrefix(e, "# branch.head "):
			if b := strings.TrimPrefix(e, "# branch.head "); b != "(detached)" {
				s.Branch = b
			}
		case strings.HasPrefix(e, "# branch.upstream "):
			s.Upstream = strings.TrimPrefix(e, "# branch.upstream ")
		case strings.HasPrefix(e, "# branch.ab "):
			fmt.Sscanf(strings.TrimPrefix(e, "# branch.ab "), "+%d -%d", &s.Ahead, &s.Behind)
		case strings.HasPrefix(e, "1 "), strings.HasPrefix(e, "2 "):
			// 1 XY sub mH mI mW hH hI path; 2 adds a score, and the source follows.
			n := 9
			if e[0] == '2' {
				n = 10
			}
			f := strings.SplitN(e, " ", n)
			if len(f) < n {
				return s, fmt.Errorf("unexpected status entry %q", e)
			}
			path, orig := f[n-1], ""
			if e[0] == '2' && i+1 < len(fields) {
				i++
				orig = fields[i]
			}
			x, y := f[1][0], f[1][1]
			if x != '.' {
				s.Files = append(s.Files, File{Path: path, Orig: orig, Kind: Staged, Code: x})
				fmt.Fprintf(index, "%c %s %s %s\x00%s\x00", x, f[4], f[7], path, orig)
			}
			if y != '.' {
				s.Files = append(s.Files, File{Path: path, Kind: Unstaged, Code: y})
			}
		case strings.HasPrefix(e, "u "):
			f := strings.SplitN(e, " ", 11)
			if len(f) < 11 {
				return s, fmt.Errorf("unexpected status entry %q", e)
			}
			s.Files = append(s.Files, File{Path: f[10], Kind: Conflict, Code: 'U'})
			fmt.Fprintf(index, "u %s\x00", f[10])
		case strings.HasPrefix(e, "? "):
			s.Files = append(s.Files, File{Path: e[2:], Kind: Untracked, Code: '?'})
		}
	}
	s.Index = hex.EncodeToString(index.Sum(nil)[:8])
	return s, nil
}

// Diff is a file's change as shown: its text, and a revision that changes
// whenever the change does, so an action on what was shown can be refused when
// the file moved on.
type Diff struct {
	Text     string
	Revision string
	Hunks    []Hunk
	Binary   bool
	Cut      bool // longer than the limit; the rest isn't shown
}

// Hunk is one @@ section of a diff, by its line range in Text.
type Hunk struct {
	Start, End int // lines of Text, End exclusive
}

// GetDiff reads f's change, at most limit bytes of it.
func GetDiff(ctx context.Context, run Run, f File, limit int) (Diff, error) {
	base := []string{"--no-ext-diff", "--no-textconv", "--no-color", "--find-renames"}
	var args []string
	switch f.Kind {
	case Staged:
		args = append(append([]string{"diff", "--cached"}, base...), "--")
		if f.Orig != "" {
			args = append(args, f.Orig)
		}
		args = append(args, f.Path)
	case Untracked:
		args = append(append([]string{"diff", "--no-index"}, base...), "--", "/dev/null", f.Path)
	default:
		args = append(append([]string{"diff"}, base...), "--", f.Path)
	}
	out, errOut, code, err := run(ctx, append([]string{"git"}, args...), append([]string{literal}, readEnv...))
	if err != nil {
		return Diff{}, err
	}
	// git diff --no-index exits 1 when the files differ, which a new file does.
	if code != 0 && !(f.Kind == Untracked && code == 1) {
		return Diff{}, fmt.Errorf("git diff: %s", firstLine(strings.TrimSpace(string(errOut)), fmt.Sprintf("exited %d", code)))
	}
	return parseDiff(out, limit), nil
}

// BranchDiff is the box's task diff, as hutch diff shows it: everything since
// the merge base with base on the remote, committed or not.
func BranchDiff(ctx context.Context, run Run, base string, limit int) (Diff, error) {
	mb, err := git(ctx, run, readEnv, "merge-base", "HEAD", "origin/"+base)
	if err != nil {
		return Diff{}, err
	}
	out, err := git(ctx, run, readEnv, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames", strings.TrimSpace(string(mb)))
	if err != nil {
		return Diff{}, err
	}
	return parseDiff(out, limit), nil
}

func parseDiff(out []byte, limit int) Diff {
	sum := sha256.Sum256(out)
	d := Diff{Revision: hex.EncodeToString(sum[:8])}
	partial := false // the cut fell inside the last hunk
	if limit > 0 && len(out) > limit {
		end := limit
		if i := bytes.LastIndexByte(out[:limit], '\n'); i >= 0 {
			end = i + 1
		}
		rest := out[end:]
		partial = !bytes.HasPrefix(rest, []byte("@@")) && !bytes.HasPrefix(rest, []byte("diff "))
		out, d.Cut = out[:end], true
	}
	d.Text = string(out)
	lines := strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "@@"):
			if n := len(d.Hunks); n > 0 && d.Hunks[n-1].End == 0 {
				d.Hunks[n-1].End = i
			}
			d.Hunks = append(d.Hunks, Hunk{Start: i})
		case strings.HasPrefix(l, "diff --git"), strings.HasPrefix(l, "diff --cc"):
			if n := len(d.Hunks); n > 0 && d.Hunks[n-1].End == 0 {
				d.Hunks[n-1].End = i
			}
		case strings.HasPrefix(l, "Binary files ") || l == "GIT binary patch":
			d.Binary = true
		}
	}
	if n := len(d.Hunks); n > 0 && d.Hunks[n-1].End == 0 {
		d.Hunks[n-1].End = len(lines)
	}
	if partial && len(d.Hunks) > 0 {
		d.Hunks = d.Hunks[:len(d.Hunks)-1]
	}
	return d
}

// Lines are a diff's lines.
func (d Diff) Lines() []string {
	if d.Text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(d.Text, "\n"), "\n")
}

// ErrStale is an action on a change that moved on since it was shown.
var ErrStale = errors.New("the file changed since it was shown; look again")

// StageFile adds a file's worktree state to the index (a deletion too).
func StageFile(ctx context.Context, run Run, f File) error {
	_, err := git(ctx, run, nil, "add", "--all", "--", f.Path)
	return err
}

// UnstageFile puts a file's index entry back to HEAD's, or removes it from the
// index of a repository with no commit yet.
func UnstageFile(ctx context.Context, run Run, f File, unborn bool) error {
	if unborn {
		_, err := git(ctx, run, nil, "rm", "--cached", "--quiet", "--", f.Path)
		return err
	}
	paths := []string{f.Path}
	if f.Orig != "" {
		paths = append(paths, f.Orig)
	}
	_, err := git(ctx, run, nil, append([]string{"restore", "--staged", "--"}, paths...)...)
	return err
}

// StageHunk stages hunk i of f's unstaged change, or with f staged, unstages
// it, if the change is still the revision shown.
func StageHunk(ctx context.Context, run Run, f File, revision string, i int) error {
	if f.Kind != Unstaged && f.Kind != Staged {
		return fmt.Errorf("a %s file is staged as a whole", f.Kind)
	}
	cur, err := GetDiff(ctx, run, f, 0)
	if err != nil {
		return err
	}
	if cur.Revision != revision {
		return ErrStale
	}
	if cur.Binary {
		return errors.New("a binary change is staged as a whole")
	}
	patch, err := cur.HunkPatch(i)
	if err != nil {
		return err
	}
	args := []string{"apply", "--cached", "--whitespace=nowarn"}
	if f.Kind == Staged {
		args = append(args, "--reverse")
	}
	// The patch is an argument, never shell text.
	argv := append([]string{"sh", "-c", `p=$1; shift; printf '%s' "$p" | git "$@"`, "sh", patch}, append(args, "-")...)
	out, errOut, code, err := run(ctx, argv, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git apply: %s", firstLine(strings.TrimSpace(string(errOut)+string(out)), fmt.Sprintf("exited %d", code)))
	}
	return nil
}

// HunkPatch is a patch of the file's header and hunk i alone. A rename or copy
// is left out of it: the hunk applies to the file under its new path, so
// unstaging one hunk never undoes the rename.
func (d Diff) HunkPatch(i int) (string, error) {
	if i < 0 || i >= len(d.Hunks) {
		return "", fmt.Errorf("no hunk %d", i+1)
	}
	lines := d.Lines()
	head := lines[:d.Hunks[0].Start]
	for _, l := range head {
		if plus, ok := strings.CutPrefix(l, "+++ "); ok && slices.ContainsFunc(head, isRenameLine) {
			minus := strings.Replace(plus, "b/", "a/", 1)
			head = []string{"diff --git " + minus + " " + plus, "--- " + minus, l}
			break
		}
	}
	h := d.Hunks[i]
	return strings.Join(head, "\n") + "\n" + strings.Join(lines[h.Start:h.End], "\n") + "\n", nil
}

func isRenameLine(l string) bool {
	return strings.HasPrefix(l, "rename from ") || strings.HasPrefix(l, "copy from ")
}

// ErrStaleIndex is a commit of an index that changed since it was shown.
var ErrStaleIndex = errors.New("what's staged changed since it was shown; look again")

// Commit commits the index with message, if it's still index (Status.Index);
// nothing else is staged or added.
func Commit(ctx context.Context, run Run, message, index string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", errors.New("write a commit message")
	}
	s, err := GetStatus(ctx, run)
	if err != nil {
		return "", err
	}
	if s.Index != index {
		return "", ErrStaleIndex
	}
	if _, err := git(ctx, run, nil, "commit", "--quiet", "--message", message); err != nil {
		return "", err
	}
	out, err := git(ctx, run, readEnv, "log", "-1", "--format=%h %s")
	return strings.TrimSpace(string(out)), err
}
