package version

import (
	"regexp"
	"strconv"
	"strings"
)

// Older reports whether release a is older than release b. Builds that aren't
// releases (dev, a commit) aren't ordered, so it's false for them. A build from
// source comes just after the version it was described from (git describe:
// v0.1.0-16-g4a5fe90, v0.2.0-rc.2-dirty), ordered by its commits past it.
func Older(a, b string) bool {
	x, okA := parse(a)
	y, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range 3 {
		if x.n[i] != y.n[i] {
			return x.n[i] < y.n[i]
		}
	}
	if x.pre != y.pre {
		return preOlder(x.pre, y.pre)
	}
	switch {
	case x.build != y.build:
		return !x.build
	case x.commits != y.commits:
		return x.commits < y.commits
	}
	return !x.dirty && y.dirty
}

// preOlder orders different pre-release identifiers; a pre-release comes
// before its release ("").
func preOlder(x, y string) bool {
	switch {
	case x == "":
		return false
	case y == "":
		return true
	}
	xp, yp := strings.Split(x, "."), strings.Split(y, ".")
	for i := 0; i < min(len(xp), len(yp)); i++ {
		if xp[i] == yp[i] {
			continue
		}
		xn, yn := numeric(xp[i]), numeric(yp[i])
		if xn != yn {
			return xn // Numeric identifiers precede nonnumeric identifiers.
		}
		if xn && len(xp[i]) != len(yp[i]) {
			return len(xp[i]) < len(yp[i])
		}
		return xp[i] < yp[i]
	}
	return len(xp) < len(yp)
}

type release struct {
	n   [3]int
	pre string
	// build: a source build described from this version, commits past it,
	// with uncommitted changes when dirty.
	build   bool
	commits int
	dirty   bool
}

// describe matches git describe's suffix: "<commits>-g<hash>".
var describe = regexp.MustCompile(`^(?:(.*)-)?([0-9]+)-g[0-9a-f]{4,}$`)

func parse(v string) (release, bool) {
	var r release
	v, ok := strings.CutPrefix(v, "v")
	if !ok {
		return r, false
	}
	v, metadata, hasMetadata := strings.Cut(v, "+")
	if hasMetadata && !identifiers(metadata, false) {
		return r, false
	}
	v, r.pre, ok = strings.Cut(v, "-")
	if ok && !identifiers(r.pre, true) {
		return r, false
	}
	if rest, dirty := strings.CutSuffix(r.pre, "dirty"); dirty && (rest == "" || strings.HasSuffix(rest, "-")) {
		r.pre, r.build, r.dirty = strings.TrimSuffix(rest, "-"), true, true
	}
	if m := describe.FindStringSubmatch(r.pre); m != nil {
		r.pre, r.build = m[1], true
		r.commits, _ = strconv.Atoi(m[2])
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return r, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || !numeric(p) || len(p) > 1 && p[0] == '0' {
			return r, false
		}
		r.n[i] = n
	}
	return r, true
}

func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func identifiers(s string, prerelease bool) bool {
	for _, part := range strings.Split(s, ".") {
		if part == "" || prerelease && numeric(part) && len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, c := range part {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-') {
				return false
			}
		}
	}
	return true
}
