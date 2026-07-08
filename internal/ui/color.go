// Package ui renders tforg's terminal output: the color palette and
// unified diffs.
package ui

import (
	"os"
	"strings"
)

// fileColorCodes assigns an ANSI SGR color to each conventional destination
// file, from the standard 16-color palette so it stays readable on both
// light and dark terminals.
var fileColorCodes = map[string]string{
	"main.tf":      "32", // green
	"data.tf":      "34", // blue
	"variables.tf": "33", // yellow
	"outputs.tf":   "36", // cyan
	"locals.tf":    "35", // magenta
	"providers.tf": "95", // bright magenta
	"versions.tf":  "90", // gray
	"moved.tf":     "94", // bright blue
	"imports.tf":   "94",
	"removed.tf":   "91", // bright red
	"checks.tf":    "96", // bright cyan
	"ephemeral.tf": "96",
}

// otherFileCodes is the pool for non-conventional file names; the color is
// derived from the name so the same file is painted consistently everywhere.
var otherFileCodes = []string{"32", "33", "34", "35", "36", "94", "95", "96"}

type Palette struct{ on bool }

// NewPalette decides whether to emit color: an explicit flag or the NO_COLOR
// convention wins, CLICOLOR_FORCE overrides the TTY check (useful when a hook
// runner captures output), otherwise color only when stdout is a terminal.
func NewPalette(disable bool) Palette {
	if disable || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return Palette{}
	}
	if f := os.Getenv("CLICOLOR_FORCE"); f != "" && f != "0" {
		return Palette{on: true}
	}
	fi, err := os.Stdout.Stat()
	return Palette{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (p Palette) Paint(code, s string) string {
	if !p.on || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p Palette) File(name string) string { return p.Paint(fileColorCode(name), name) }
func (p Palette) Bold(s string) string    { return p.Paint("1", s) }
func (p Palette) Dim(s string) string     { return p.Paint("2", s) }
func (p Palette) Red(s string) string     { return p.Paint("31", s) }
func (p Palette) Green(s string) string   { return p.Paint("32", s) }
func (p Palette) Yellow(s string) string  { return p.Paint("33", s) }

func fileColorCode(name string) string {
	if c, ok := fileColorCodes[strings.ToLower(name)]; ok {
		return c
	}
	h := 0
	for _, r := range name {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return otherFileCodes[h%len(otherFileCodes)]
}
