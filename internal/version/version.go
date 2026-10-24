package version

import "runtime/debug"

// Version is set with -ldflags "-X github.com/mouizahmed/hutch/internal/version.Version=...".
var Version = ""

func String() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
