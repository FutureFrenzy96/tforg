package ui

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
	p := Palette{}
	if got := p.File("main.tf"); got != "main.tf" {
		t.Errorf("disabled palette painted: %q", got)
	}
	if got := p.Bold("x"); got != "x" {
		t.Errorf("disabled palette painted: %q", got)
	}
}

func TestEnabledPaletteWrapsWithSGR(t *testing.T) {
	p := Palette{on: true}
	got := p.File("main.tf")
	if !strings.HasPrefix(got, "\x1b[32m") || !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("unexpected escape wrapping: %q", got)
	}
}

func TestNoColorEnvWins(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "1")
	if NewPalette(false).on {
		t.Error("NO_COLOR must disable color even when CLICOLOR_FORCE is set")
	}
}

func TestClicolorForceEnables(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("CLICOLOR_FORCE", "1")
	if !NewPalette(false).on {
		t.Error("CLICOLOR_FORCE should enable color without a TTY")
	}
	if NewPalette(true).on {
		t.Error("-no-color flag must win over CLICOLOR_FORCE")
	}
}

func TestUnifiedDiffLines(t *testing.T) {
	lines := unifiedDiffLines("x/main.tf", []byte("a = 1\nb = 2\n"), []byte("a = 1\nb = 3\n"))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"--- a/x/main.tf", "+++ b/x/main.tf", "-b = 2", "+b = 3"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff missing %q:\n%s", want, joined)
		}
	}
	if unifiedDiffLines("x", []byte("same\n"), []byte("same\n")) != nil {
		t.Error("identical content should produce no diff")
	}
}
