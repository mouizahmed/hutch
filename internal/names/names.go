// Package names generates box names like quiet-fox.
package names

import (
	"math/rand/v2"
	"strconv"
)

var adjectives = []string{
	"quiet", "brave", "swift", "calm", "bold", "bright", "clever", "eager", "gentle", "happy",
	"keen", "lively", "lucky", "merry", "nimble", "proud", "quick", "sharp", "steady", "sunny",
	"tidy", "warm", "wise", "zesty", "amber", "azure", "coral", "crimson", "golden", "ivory",
	"jade", "misty", "rosy", "silver", "velvet", "frosty", "breezy", "dusky", "plucky", "spry",
}

var nouns = []string{
	"fox", "owl", "hare", "otter", "lynx", "wren", "finch", "badger", "heron", "robin",
	"marten", "stoat", "vole", "newt", "moth", "crane", "falcon", "raven", "bison", "panda",
	"koala", "lemur", "gecko", "ibis", "kite", "mole", "puffin", "quail", "seal", "tern",
	"yak", "zebra", "beetle", "dingo", "egret", "ferret", "gopher", "hedgehog", "jackal", "magpie",
}

// New returns a name not in taken.
func New(taken map[string]bool) string {
	for i := 0; ; i++ {
		n := adjectives[rand.IntN(len(adjectives))] + "-" + nouns[rand.IntN(len(nouns))]
		if i >= 100 {
			n += "-" + strconv.Itoa(rand.IntN(1000))
		}
		if !taken[n] {
			return n
		}
	}
}
