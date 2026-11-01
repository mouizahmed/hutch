package durable

import (
	"fmt"
	"strconv"
	"strings"
)

// A box's ownership number, "<term>.<n>", is raised by the lead each time it
// places the box on another machine, and is part of its manifests' names, so two
// copies of one box never write the same object. "" is 0.0: boxes from before
// format 2, whose manifests are named by generation alone.

// ManifestName is where generation gen of a box (or template version) with
// prefix and owner is in the bucket.
func ManifestName(prefix, owner string, gen int) string {
	if owner == "" {
		return fmt.Sprintf("%s/%010d", prefix, gen)
	}
	return fmt.Sprintf("%s/%s-%010d", prefix, owner, gen)
}

// ParseManifestName reads the owner and generation from a manifest's name after
// its prefix.
func ParseManifestName(rest string) (owner string, gen int, ok bool) {
	g := rest
	if o, after, found := strings.Cut(rest, "-"); found {
		if _, _, ok := parseOwner(o); !ok {
			return "", 0, false
		}
		owner, g = o, after
	}
	n, err := strconv.Atoi(g)
	return owner, n, err == nil && n > 0
}

func parseOwner(s string) (term, n int64, ok bool) {
	if s == "" {
		return 0, 0, true
	}
	t, nn, found := strings.Cut(s, ".")
	term, err1 := strconv.ParseInt(t, 10, 64)
	n, err2 := strconv.ParseInt(nn, 10, 64)
	return term, n, found && err1 == nil && err2 == nil && term >= 0 && n > 0
}

// OwnerLess compares ownership numbers, term first.
func OwnerLess(a, b string) bool {
	at, an, _ := parseOwner(a)
	bt, bn, _ := parseOwner(b)
	return at < bt || at == bt && an < bn
}

// Owner formats an ownership number.
func Owner(term, n int64) string { return fmt.Sprintf("%d.%d", term, n) }

// Newer orders two manifests of one box: the higher owner, then generation.
func Newer(owner string, gen int, thanOwner string, thanGen int) bool {
	if owner != thanOwner {
		return OwnerLess(thanOwner, owner)
	}
	return gen > thanGen
}
