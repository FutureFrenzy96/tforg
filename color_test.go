package main

import (
	"strings"
	"testing"
)

func TestConventionalFilesHaveFixedColors(t *testing.T) {
	for name := range fileColorCodes {
		if fileColorCode(name) != fileColorCodes[name] {
			t.Errorf("%s: expected fixed color %s", name, fileColorCodes[name])
		}
	}
	if fileColorCode("MAIN.TF") != fileColorCodes["main.tf"] {
		t.Error("file color lookup should be case-insensitive")
	}
}

func TestOtherFilesGetStableColor(t *testing.T) {
	a, b := fileColorCode("everything.tf"), fileColorCode("everything.tf")
	if a != b {
		t.Errorf("color not stable: %s vs %s", a, b)
	}
	found := false
	for _, c := range otherFileCodes {
		if c == a {
			found = true
		}
	}
	if !found {
		t.Errorf("color %s not from the fallback pool", a)
	}
}

func TestDisabledPaletteEmitsPlainText(t *testing.T) {
	p := palette{}
	if got := p.file("main.tf"); got != "main.tf" {
		t.Errorf("disabled palette painted: %q", got)
	}
	if got := p.bold("x"); got != "x" {
		t.Errorf("disabled palette painted: %q", got)
	}
}

func TestEnabledPaletteWrapsWithSGR(t *testing.T) {
	p := palette{on: true}
	got := p.file("main.tf")
	if !strings.HasPrefix(got, "\x1b[32m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("unexpected escape wrapping: %q", got)
	}
}

func TestNoColorEnvWins(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "1")
	if newPalette(false).on {
		t.Error("NO_COLOR must disable color even when CLICOLOR_FORCE is set")
	}
}

func TestClicolorForceEnables(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CLICOLOR_FORCE", "1")
	if !newPalette(false).on {
		t.Error("CLICOLOR_FORCE should enable color without a TTY")
	}
	if newPalette(true).on {
		t.Error("-no-color flag must win over CLICOLOR_FORCE")
	}
}
