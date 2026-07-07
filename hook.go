package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const hookScript = `#!/bin/sh
# Installed by tforg install-hook.
# Formats and organizes staged Terraform files; aborts the commit when files
# are rewritten so the changes can be reviewed and re-staged.
exec tforg -staged
`

// installHook writes a pre-commit hook into the repository containing the
// current directory, honoring core.hooksPath.
func installHook(args []string) int {
	force := false
	for _, a := range args {
		if a == "-force" || a == "--force" {
			force = true
			continue
		}
		fmt.Fprintf(os.Stderr, "install-hook: unknown argument %q\n", a)
		return 2
	}
	pal := newPalette(false)

	hooksDir, err := gitOutput("rev-parse", "--git-path", "hooks")
	if err != nil {
		fmt.Fprintln(os.Stderr, pal.red("✗"), "install-hook requires a git repository:", err)
		return 2
	}
	path := filepath.Join(hooksDir, "pre-commit")
	if _, err := os.Stat(path); err == nil && !force {
		fmt.Fprintln(os.Stderr, pal.red("✗"), path, "already exists (use `tforg install-hook -force` to overwrite)")
		return 2
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, pal.red("✗"), err)
		return 2
	}
	if err := os.WriteFile(path, []byte(hookScript), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, pal.red("✗"), err)
		return 2
	}
	fmt.Println(pal.green("✓"), "installed", path)
	return 0
}
