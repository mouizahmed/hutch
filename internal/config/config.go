// Package config loads the user config, ~/.config/hutch/config.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type User struct {
	Version int `toml:"version"`
	// Server is the SSH target of the server, e.g. "me@home-server". Empty means a
	// server running on this machine as the current user.
	Server string `toml:"server"`
}

func Path() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "hutch", "config.toml")
}

// Load reads the user config. A missing file is an empty config; unknown keys are errors.
func Load() (User, error) {
	var u User
	md, err := toml.DecodeFile(Path(), &u)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, fmt.Errorf("%s: %w", Path(), err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return u, fmt.Errorf("%s: unknown keys: %s", Path(), strings.Join(keys, ", "))
	}
	return u, nil
}
