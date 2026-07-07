package main

import "runtime/debug"

// version is set by releases via -ldflags "-X main.version=v1.2.3"; when
// installed with `go install module@version`, the module version from build
// info is used instead.
var version string

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
