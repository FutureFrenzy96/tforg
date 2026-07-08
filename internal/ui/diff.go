package ui

import (
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// unifiedDiffLines renders a git-style unified diff between two file
// contents; nil content stands for a missing file (creation or deletion).
func unifiedDiffLines(relPath string, a, b []byte) []string {
	ud := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(a)),
		B:        difflib.SplitLines(string(b)),
		FromFile: "a/" + relPath,
		ToFile:   "b/" + relPath,
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(ud)
	if err != nil || text == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// PrintDiff writes a colorized unified diff to stdout.
func PrintDiff(pal Palette, relPath string, a, b []byte) {
	for _, line := range unifiedDiffLines(relPath, a, b) {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			fmt.Println(pal.Bold(line))
		case strings.HasPrefix(line, "@@"):
			fmt.Println(pal.Paint("36", line))
		case strings.HasPrefix(line, "+"):
			fmt.Println(pal.Green(line))
		case strings.HasPrefix(line, "-"):
			fmt.Println(pal.Red(line))
		default:
			fmt.Println(pal.Dim(line))
		}
	}
}
