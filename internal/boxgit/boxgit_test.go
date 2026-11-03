package boxgit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// local runs Git in dir, as a box's exec would in /workspace.
func local(dir string) Run {
	return func(ctx context.Context, argv, env []string) ([]byte, []byte, int, error) {
		c := exec.CommandContext(ctx, argv[0], argv[1:]...)
		c.Dir = dir
		c.Env = append(os.Environ(), append([]string{"GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir}, env...)...)
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		err := c.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.Bytes(), errOut.Bytes(), exit.ExitCode(), nil
		}
		return out.Bytes(), errOut.Bytes(), 0, err
	}
}

func repo(t *testing.T) (string, Run) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := local(dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
		if _, err := git(t.Context(), run, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir, run
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func find(s Status, path string, k Kind) (File, bool) {
	for _, f := range s.Files {
		if f.Path == path && f.Kind == k {
			return f, true
		}
	}
	return File{}, false
}

// A repository with no commit yet: new files are untracked, then staged, and
// unstaging one doesn't need a HEAD.
func TestUnborn(t *testing.T) {
	ctx := t.Context()
	dir, run := repo(t)
	write(t, dir, "a b.txt", "one\n")
	s, err := GetStatus(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := find(s, "a b.txt", Untracked)
	if !s.Unborn || s.Branch != "main" || !ok {
		t.Fatalf("status: %+v", s)
	}
	d, err := GetDiff(ctx, run, f, 0)
	if err != nil || !strings.Contains(d.Text, "+one") {
		t.Fatalf("untracked diff: %q %v", d.Text, err)
	}
	if err := StageFile(ctx, run, f); err != nil {
		t.Fatal(err)
	}
	s, _ = GetStatus(ctx, run)
	f, ok = find(s, "a b.txt", Staged)
	if !ok || !s.Staged() {
		t.Fatalf("after staging: %+v", s)
	}
	if err := UnstageFile(ctx, run, f, s.Unborn); err != nil {
		t.Fatal(err)
	}
	if s, _ = GetStatus(ctx, run); s.Staged() {
		t.Fatalf("after unstaging: %+v", s)
	}
}

// One file staged and then edited again shows in both groups; a hunk stages on
// its own; a hunk of a file that changed since it was shown is refused; reading
// writes nothing.
func TestHunksAndStale(t *testing.T) {
	ctx := t.Context()
	dir, run := repo(t)
	var lines []string
	for i := range 30 {
		lines = append(lines, "line "+string(rune('a'+i%26)))
	}
	write(t, dir, "f.txt", strings.Join(lines, "\n")+"\n")
	write(t, dir, "old.txt", "moving\n")
	write(t, dir, "bin.dat", "\x00\x01\x02")
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "first"}} {
		if _, err := git(ctx, run, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Two separate edits: two hunks.
	lines[1], lines[25] = "changed near the top", "changed near the end"
	write(t, dir, "f.txt", strings.Join(lines, "\n")+"\n")
	if _, err := git(ctx, run, nil, "mv", "old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "bin.dat", "\x00\x03")

	index := filepath.Join(dir, ".git", "index")
	before, _ := os.Stat(index)
	s, err := GetStatus(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := find(s, "f.txt", Unstaged)
	d, err := GetDiff(ctx, run, f, 0)
	if err != nil || len(d.Hunks) != 2 {
		t.Fatalf("diff: %d hunks, %v\n%s", len(d.Hunks), err, d.Text)
	}
	if after, _ := os.Stat(index); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("reading status and a diff wrote the index")
	}
	if r, ok := find(s, "new.txt", Staged); !ok || r.Orig != "old.txt" || r.Code != 'R' {
		t.Fatalf("rename: %+v", s.Files)
	}
	b, _ := find(s, "bin.dat", Unstaged)
	if bd, err := GetDiff(ctx, run, b, 0); err != nil || !bd.Binary || len(bd.Hunks) != 0 {
		t.Fatalf("binary: %+v %v", bd, err)
	}

	if err := StageHunk(ctx, run, f, d.Revision, 0); err != nil {
		t.Fatal(err)
	}
	s, _ = GetStatus(ctx, run)
	staged, ok1 := find(s, "f.txt", Staged)
	_, ok2 := find(s, "f.txt", Unstaged)
	if !ok1 || !ok2 {
		t.Fatalf("one hunk staged: want f.txt in both groups: %+v", s.Files)
	}
	sd, _ := GetDiff(ctx, run, staged, 0)
	if !strings.Contains(sd.Text, "near the top") || strings.Contains(sd.Text, "near the end") {
		t.Fatalf("staged diff:\n%s", sd.Text)
	}

	// The worktree moves on: the revision shown no longer applies.
	f, _ = find(s, "f.txt", Unstaged)
	shown, _ := GetDiff(ctx, run, f, 0)
	lines[25] = "edited again"
	write(t, dir, "f.txt", strings.Join(lines, "\n")+"\n")
	if err := StageHunk(ctx, run, f, shown.Revision, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("a stale hunk: %v", err)
	}

	// Unstaging the staged hunk puts the index back.
	if err := StageHunk(ctx, run, staged, sd.Revision, 0); err != nil {
		t.Fatal(err)
	}
	s, _ = GetStatus(ctx, run)
	if _, ok := find(s, "f.txt", Staged); ok {
		t.Fatalf("after unstaging the hunk: %+v", s.Files)
	}

	if _, err := Commit(ctx, run, "  "); err == nil {
		t.Fatal("an empty message committed")
	}
	got, err := Commit(ctx, run, "move old to new")
	if err != nil || !strings.HasSuffix(got, "move old to new") {
		t.Fatalf("commit: %q %v", got, err)
	}
	s, _ = GetStatus(ctx, run)
	if s.Staged() {
		t.Fatal("the commit left something staged")
	}
	if _, ok := find(s, "f.txt", Unstaged); !ok {
		t.Fatal("the commit took unstaged work")
	}
}

// A long diff is cut at the limit, and its last, partial hunk isn't offered.
func TestDiffLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/x b/x\n--- a/x\n+++ b/x\n")
	for range 3 {
		b.WriteString("@@ -1 +1 @@\n-a\n+b\n")
	}
	// Cut between hunks: the complete ones stay.
	if d := parseDiff([]byte(b.String()), 60); !d.Cut || len(d.Hunks) != 1 {
		t.Fatalf("cut between hunks: %d hunks, cut %v", len(d.Hunks), d.Cut)
	}
	// Cut inside the second hunk: only the first stays.
	if d := parseDiff([]byte(b.String()), 64); !d.Cut || len(d.Hunks) != 1 {
		t.Fatalf("cut inside a hunk: %d hunks, cut %v", len(d.Hunks), d.Cut)
	}
	if d := parseDiff([]byte(b.String()), 0); d.Cut || len(d.Hunks) != 3 {
		t.Fatalf("no limit: %d hunks", len(d.Hunks))
	}
}

// Unstaging one hunk of a renamed, edited file keeps the rename staged.
func TestUnstageHunkOfRename(t *testing.T) {
	ctx := t.Context()
	dir, run := repo(t)
	var lines []string
	for i := range 12 {
		lines = append(lines, "line "+string(rune('a'+i)))
	}
	write(t, dir, "a.txt", strings.Join(lines, "\n")+"\n")
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "first"}, {"mv", "a.txt", "b.txt"}} {
		if _, err := git(ctx, run, nil, args...); err != nil {
			t.Fatal(err)
		}
	}
	lines[0] = "edited"
	write(t, dir, "b.txt", strings.Join(lines, "\n")+"\n")
	if _, err := git(ctx, run, nil, "add", "b.txt"); err != nil {
		t.Fatal(err)
	}
	s, _ := GetStatus(ctx, run)
	f, ok := find(s, "b.txt", Staged)
	if !ok || f.Orig != "a.txt" {
		t.Fatalf("want a staged rename: %+v", s.Files)
	}
	d, err := GetDiff(ctx, run, f, 0)
	if err != nil || len(d.Hunks) != 1 {
		t.Fatalf("diff: %v\n%s", err, d.Text)
	}
	if err := StageHunk(ctx, run, f, d.Revision, 0); err != nil {
		t.Fatal(err)
	}
	s, _ = GetStatus(ctx, run)
	if r, ok := find(s, "b.txt", Staged); !ok || r.Orig != "a.txt" {
		t.Fatalf("the rename was undone: %+v", s.Files)
	}
	if _, ok := find(s, "b.txt", Unstaged); !ok {
		t.Fatalf("the edit should be unstaged now: %+v", s.Files)
	}
}
