//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/wire"
)

func writeFiles(req guestproto.Files) error {
	name := req.User
	if name == "" {
		name = "dev"
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, f := range req.Files {
		path := f.Path
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			path = filepath.Join(u.HomeDir, rest)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s: path must be absolute or start with ~/", f.Path)
		}
		data := f.Data
		if f.Merge {
			if data, err = mergeFile(path, f.Data); err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
		}
		if err := mkdirAllOwned(filepath.Dir(path), uid, gid); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o600
		}
		tmp := path + ".hutch-tmp"
		if err := os.WriteFile(tmp, data, mode); err != nil {
			return err
		}
		if err := os.Chmod(tmp, mode); err != nil {
			return err
		}
		if err := os.Chown(tmp, uid, gid); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAllOwned creates missing directories owned by the user.
func mkdirAllOwned(dir string, uid, gid int) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := mkdirAllOwned(filepath.Dir(dir), uid, gid); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chown(dir, uid, gid)
}

// mergeFile deep-merges patch into the file at path (missing means empty).
func mergeFile(path string, patch []byte) ([]byte, error) {
	cur, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	switch filepath.Ext(path) {
	case ".json":
		base, err := decodeJSON(cur)
		if err != nil {
			return nil, err
		}
		p, err := decodeJSON(patch)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(merge(base, p), "", "  ")
	case ".toml":
		base, p := map[string]any{}, map[string]any{}
		if _, err := toml.Decode(string(cur), &base); err != nil {
			return nil, err
		}
		if _, err := toml.Decode(string(patch), &p); err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		err := toml.NewEncoder(&buf).Encode(merge(base, p))
		return buf.Bytes(), err
	}
	return nil, fmt.Errorf("can only merge .json and .toml files")
}

func decodeJSON(b []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return m, d.Decode(&m)
}

func merge(base, patch map[string]any) map[string]any {
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := base[k].(map[string]any); ok {
				base[k] = merge(bm, pm)
				continue
			}
		}
		base[k] = v
	}
	return base
}

// dial connects the host to a port on the guest's loopback.
func dial(conn net.Conn, d guestproto.Dial) {
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", d.Port))
	if err != nil {
		if c, err = net.Dial("tcp", fmt.Sprintf("[::1]:%d", d.Port)); err != nil {
			(&responder{conn: conn}).fail(err)
			return
		}
	}
	defer c.Close()
	if err := wire.Write(conn, guestproto.Response{Type: guestproto.RespOK}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, conn); done <- struct{}{} }()
	go func() { io.Copy(conn, c); done <- struct{}{} }()
	<-done
}
