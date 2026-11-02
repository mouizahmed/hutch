package server

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mouizahmed/hutch/internal/config"
)

// Every setting the server reads is in the schema: reading one that isn't
// panics, and only when that code runs.
func TestSettingsReadAreKnown(t *testing.T) {
	read := regexp.MustCompile(`\.(Duration|Int|String|Size|Percent|Float|List)\("([a-z_]+\.[a-z_.]+|[a-z_]+)"\)`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range read.FindAllStringSubmatch(string(src), -1) {
			p, err := config.SplitPath(m[2])
			if err != nil || config.Lookup(p) == nil {
				t.Errorf("%s reads %s, which isn't in the schema", f, m[2])
			}
		}
	}
}
