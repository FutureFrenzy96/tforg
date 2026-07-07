package main

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

type palette struct{ on bool }

// newPalette decides whether to emit color: an explicit flag or the NO_COLOR
// convention wins, CLICOLOR_FORCE overrides the TTY check (useful when a hook
// runner captures output), otherwise color only when stdout is a terminal.
func newPalette(disable bool) palette {
	if disable || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	if f := os.Getenv("CLICOLOR_FORCE"); f != "" && f != "0" {
		return palette{on: true}
	}
	fi, err := os.Stdout.Stat()
	return palette{on: err == nil && fi.Mode()&os.ModeCharDevice != 0}
}

func (p palette) paint(code, s string) string {
	if !p.on || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p palette) file(name string) string { return p.paint(fileColorCode(name), name) }
func (p palette) bold(s string) string    { return p.paint("1", s) }
func (p palette) dim(s string) string     { return p.paint("2", s) }
func (p palette) red(s string) string     { return p.paint("31", s) }
func (p palette) green(s string) string   { return p.paint("32", s) }
func (p palette) yellow(s string) string  { return p.paint("33", s) }

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
