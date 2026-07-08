// Package gitx holds tforg's git integration: staged-file discovery and the
// pre-commit hook installer.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// StagedTfFiles returns absolute paths of the .tf files currently staged for
// commit (added, copied, modified, or renamed).
func StagedTfFiles() ([]string, []string) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, []string{"-staged requires a git repository: " + err.Error()}
	}
	// -C anchors the pathspec at the repo root so staged files are found
	// no matter which subdirectory the hook runs from.
	out, err := gitOutput("-C", root, "diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z", "--", "*.tf")
	if err != nil {
		return nil, []string{err.Error()}
	}
	var files []string
	for _, name := range strings.Split(out, "\x00") {
		if name == "" || !strings.HasSuffix(name, ".tf") {
			continue
		}
		files = append(files, filepath.Join(root, filepath.FromSlash(name)))
	}
	return files, nil
}
