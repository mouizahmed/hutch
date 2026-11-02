package version

import (
	"strconv"
	"strings"
)

// Older reports whether release a is older than release b. Builds that aren't
// releases (dev, a commit) aren't ordered, so it's false for them.
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
	// A build from source, described as N commits past a release
	// (v0.1.0-16-g4a5fe90), comes after that release.
	if xn, ok := past(x.pre); ok {
		yn, ok := past(y.pre)
		return ok && xn < yn
	}
	if _, ok := past(y.pre); ok {
		return true
	}
	// A pre-release comes before its release.
	switch {
	case x.pre == y.pre:
		return false
	case x.pre == "":
		return false
	case y.pre == "":
		return true
	}
	xp, yp := strings.Split(x.pre, "."), strings.Split(y.pre, ".")
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
}

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

// past reads git describe's "<commits>-g<hash>[-dirty]" after a release.
func past(pre string) (int, bool) {
	n, rest, ok := strings.Cut(pre, "-g")
	if !ok {
		return 0, false
	}
	hash := strings.TrimSuffix(rest, "-dirty")
	commits, err := strconv.Atoi(n)
	if err != nil || len(hash) < 4 || strings.Trim(hash, "0123456789abcdef") != "" {
		return 0, false
	}
	return commits, true
}
