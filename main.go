// Command tforg formats Terraform files (terraform fmt equivalent) and
// organizes top-level blocks into their conventional files. See internal/cli
// for the command implementation and internal/engine for the core logic.
package main

import (
	"os"

	"github.com/FutureFrenzy96/tforg/internal/cli"
)

// version is set by releases via -ldflags "-X main.version=v1.2.3".
var version string

func main() {
	os.Exit(cli.Run(os.Args[1:], version))
}
