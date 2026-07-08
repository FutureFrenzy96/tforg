package cli

import "runtime/debug"

// versionString prefers the release version stamped via ldflags; when
// installed with `go install module@version`, the module version from build
// info is used instead.
func versionString(override string) string {
	if override != "" {
		return override
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
