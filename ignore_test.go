package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchesIgnore(t *testing.T) {
	base := "/repo"
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"**/generated/**", "/repo/modules/generated/vpc/main.tf", true},
		{"**/generated/**", "/repo/modules/vpc/main.tf", false},
		{"*.gen.tf", "/repo/modules/vpc/net.gen.tf", true}, // bare: base name
		{"*.gen.tf", "/repo/modules/vpc/main.tf", false},
		{"generated", "/repo/generated/main.tf", true}, // bare: any component
		{"generated", "/repo/modules/generated/x/main.tf", true},
		{"generated", "/repo/modules/vpc/main.tf", false},
		{"modules/legacy/**", "/repo/modules/legacy/a/b.tf", true},
		{"modules/legacy/**", "/repo/modules/current/b.tf", false},
	}
	for _, c := range cases {
		if got := matchesIgnore([]string{c.pattern}, base, filepath.FromSlash(c.file)); got != c.want {
			t.Errorf("pattern %q vs %q: got %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
	if matchesIgnore(nil, base, "/repo/main.tf") {
		t.Error("empty pattern list must match nothing")
	}
}

func TestParseIgnoreList(t *testing.T) {
	rc, err := parseRepoConfig([]byte(`ignore = ["**/generated/**", "*.gen.tf"]`), ".tforg.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.ignore) != 2 || rc.ignore[0] != "**/generated/**" {
		t.Errorf("ignore list wrong: %v", rc.ignore)
	}

	for name, src := range map[string]string{
		"not a list":   `ignore = "generated"`,
		"non-string":   `ignore = [true]`,
		"bad pattern":  `ignore = ["[unclosed"]`,
		"unknown attr": `ignored = ["x"]`,
	} {
		if _, err := parseRepoConfig([]byte(src), ".tforg.hcl"); err == nil {
			t.Errorf("%s: expected error for %q", name, src)
		}
	}
}

func TestDuplicateLocalsKeysDetected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf": `locals {
  team = "platform"
  env  = "prod"
}

resource "null_resource" "r" {
}
`,
		"locals.tf": `locals {
  region = "eu-west-1"
  team   = "data"
}
`,
	})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.errs) != 1 {
		t.Fatalf("expected one duplicate error, got %v", o.errs)
	}
	e := o.errs[0]
	if !strings.Contains(e, `duplicate local "team"`) {
		t.Errorf("wrong error: %s", e)
	}
	if !strings.Contains(e, "main.tf:2") || !strings.Contains(e, "locals.tf:3") {
		t.Errorf("error should carry file:line locations: %s", e)
	}
	if len(o.moves) != 0 {
		t.Error("moves must be aborted on duplicate locals keys")
	}
}

func TestDuplicateErrorsIncludeLineNumbers(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf":  "resource \"x\" \"y\" {\n}\n\nvariable \"region\" {\n  type = string\n}\n",
		"extra.tf": "variable \"region\" {\n  type = number\n}\n",
	})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.errs) == 0 || !strings.Contains(o.errs[0], "main.tf:4") || !strings.Contains(o.errs[0], "extra.tf:1") {
		t.Errorf("expected file:line locations, got %v", o.errs)
	}
}

func TestDuplicateLocalsIntoExistingDestination(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf": `locals {
  team = "platform"
}
`,
		"locals.tf": `locals {
  team = "data"
}
`,
	})

	// Single-file targeting: locals.tf is not in the target set.
	o := processDir(dir, []string{"main.tf"}, defaultCfg())
	if len(o.errs) == 0 || !strings.Contains(o.errs[0], `duplicate local "team"`) {
		t.Fatalf("expected duplicate local error, got %v", o.errs)
	}
	if !strings.Contains(o.errs[0], "main.tf:2") || !strings.Contains(o.errs[0], "locals.tf:2") {
		t.Errorf("expected file:line on both sides: %v", o.errs)
	}
}
