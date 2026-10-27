package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Guest frames are limited to 4 MB and file data travels base64-encoded, so calls
// carry under 2 MB of data; larger files go in parts joined in the box.
const filesBatch = 2 << 20

// writeFiles writes files of any size into a box as dev.
func (s *Server) writeFiles(ctx context.Context, w *workerConn, box string, files []guestproto.File) error {
	var small []guestproto.File
	for _, f := range files {
		if len(f.Data) < filesBatch {
			small = append(small, f)
			continue
		}
		var parts []string
		for i := 0; i*filesBatch < len(f.Data); i++ {
			part := fmt.Sprintf("/tmp/hutch-parts/%d", i)
			data := f.Data[i*filesBatch : min((i+1)*filesBatch, len(f.Data))]
			if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{Files: []guestproto.File{{Path: part, Mode: 0o600, Data: data}}}}, nil); err != nil {
				return err
			}
			parts = append(parts, part)
		}
		join := `dst=${1/#\~/$HOME}; mode=$2; shift 2; mkdir -p "$(dirname "$dst")" && cat "$@" > "$dst.hutch-tmp" && chmod "$mode" "$dst.hutch-tmp" && mv "$dst.hutch-tmp" "$dst" && rm -f "$@"`
		mode := f.Mode
		if mode == 0 {
			mode = 0o600
		}
		if err := s.run(ctx, w, box, api.ExecParams{Argv: append([]string{"bash", "-c", join, "join", f.Path, fmt.Sprintf("%o", mode)}, parts...)}, io.Discard, io.Discard); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	for len(small) > 0 {
		n, size := 0, 0
		for n < len(small) && (n == 0 || size+len(small[n].Data) < filesBatch) {
			size += len(small[n].Data)
			n++
		}
		if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{Files: small[:n]}}, nil); err != nil {
			return err
		}
		small = small[n:]
	}
	return nil
}

const deltaDir = "/tmp/hutch-delta"

func (s *Server) writeDelta(ctx context.Context, w *workerConn, box string, d *api.Delta) error {
	var files []guestproto.File
	for name, data := range map[string][]byte{"bundle": d.Bundle, "staged": d.Staged, "unstaged": d.Unstaged, "files": d.Files} {
		if len(data) > 0 {
			files = append(files, guestproto.File{Path: deltaDir + "/" + name, Mode: 0o600, Data: data})
		}
	}
	return s.writeFiles(ctx, w, box, files)
}

// takeOverBranch puts a box's /workspace on a branch from a laptop at commit, with
// the laptop's delta applied, and sets the guest to push it. The default branch is
// refused: the box would push its agents' commits straight to it.
func (s *Server) takeOverBranch(ctx context.Context, w *workerConn, box string, p api.ForkParams, stderr io.Writer) (string, string, error) {
	if err := s.writeDelta(ctx, w, box, p.Delta); err != nil {
		return "", "", err
	}
	var out bytes.Buffer
	if err := s.run(ctx, w, box, api.ExecParams{Dir: workspace, Argv: []string{"sh", "-ec", `git fetch -q origin
base=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's,^origin/,,')
if [ "$1" = "$base" ]; then echo "$1 is the default branch; the box would push to it. Switch to a branch of your own first (git switch -c <name>)." >&2; exit 3; fi
d=` + deltaDir + `
if [ -s $d/bundle ]; then git fetch -q $d/bundle HEAD; fi
git checkout -q --no-track -B "$1" "$2"
if [ -s $d/staged ]; then git apply --index $d/staged; fi
if [ -s $d/unstaged ]; then git apply $d/unstaged; fi
if [ -s $d/files ]; then tar -xf $d/files; fi
rm -rf $d
echo "$base"`, "sh", p.Branch, p.Commit}}, &out, stderr); err != nil {
		return "", "", fmt.Errorf("apply your changes: %w", err)
	}
	base := strings.TrimSpace(out.String())
	return p.Branch, base, s.setPush(ctx, w, box, p.Branch, base, p.PushInterval)
}

// setPush records a box's branch and has the guest push it after commits; an empty
// branch stops pushing.
func (s *Server) setPush(ctx context.Context, w *workerConn, box, branch, base string, interval time.Duration) error {
	if err := s.store.setBoxGit(box, branch, base); err != nil {
		return err
	}
	conf, _ := json.Marshal(guestproto.PushConfig{Dir: workspace, Branch: branch, Interval: interval})
	if err := rpc.Call(ctx, w.sess, api.BoxFiles, api.FilesParams{ID: box, Files: guestproto.Files{User: "root",
		Files: []guestproto.File{{Path: guestproto.PushConfigPath, Mode: 0o644, Data: conf}}}}, nil); err != nil {
		return fmt.Errorf("push config: %w", err)
	}
	return nil
}

// extractFiles unpacks a delta's tar into /workspace: a directory outside a project.
func (s *Server) extractFiles(ctx context.Context, w *workerConn, box string, d *api.Delta, stderr io.Writer) error {
	if err := s.writeDelta(ctx, w, box, d); err != nil {
		return err
	}
	return s.run(ctx, w, box, api.ExecParams{Dir: workspace, Argv: []string{"sh", "-ec", `tar -xf "$1/files" && rm -rf "$1"`, "sh", deltaDir}}, io.Discard, stderr)
}

// linkPath makes /workspace also appear at the laptop directory it came from (a bind
// mount, so programs see that path as their working directory), and records it for
// threads to start in. Agents key their sessions and history by that path, so they
// read the same in the box. A path the box already uses is skipped.
func (s *Server) linkPath(ctx context.Context, w *workerConn, box, path string, stderr io.Writer) error {
	if path == "" || path == workspace {
		return nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\n\t\x00") {
		return fmt.Errorf("invalid path %q", path)
	}
	fstab := strings.ReplaceAll(path, " ", `\040`)
	var msg bytes.Buffer
	err := s.run(ctx, w, box, api.ExecParams{User: "root", Argv: []string{"sh", "-ec", `p=$1
if mountpoint -q "$p"; then exit 0; fi
if [ -e "$p" ] && { [ ! -d "$p" ] || [ -n "$(ls -A "$p")" ]; }; then echo "$p is already in use in the box" >&2; exit 3; fi
mkdir -p "$p"
mount --bind ` + workspace + ` "$p"
grep -qF "` + workspace + ` $2 " /etc/fstab || echo "` + workspace + ` $2 none bind 0 0" >> /etc/fstab`, "sh", path, fstab}}, io.Discard, &msg)
	if err != nil {
		fmt.Fprintf(stderr, "warning: %s; threads start in %s instead\n", strings.TrimSpace(msg.String()), workspace)
		return nil
	}
	return s.store.setBoxPath(box, path)
}

// threadDir is where a thread starts: dir if it is within the box's path, else the path.
func threadDir(b api.Box, dir string) (string, error) {
	root := b.Path
	if root == "" {
		root = workspace
	}
	if dir == "" || dir == root {
		return root, nil
	}
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." || strings.HasPrefix(rel, "../") || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s is outside the box's %s", dir, root)
	}
	return dir, nil
}

func (s *Server) boxRelease(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.IDParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	b, err := s.store.box(p.ID)
	if err != nil {
		return nil, err
	}
	if b.Branch == "" || strings.HasPrefix(b.Branch, "hutch/") {
		return nil, errors.New("only a branch a box took over with hutch cloud is released")
	}
	w, err := s.awake(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	s.store.event(p.ID, api.BoxRelease, b.Branch)
	return nil, s.setPush(ctx, w, p.ID, "", b.Base, 0)
}
