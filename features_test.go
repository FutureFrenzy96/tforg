package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDuplicateBlocksDetected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf": `variable "region" {
  type = string
}

resource "null_resource" "r" {
}
`,
		"extra.tf": `variable "region" {
  type = number
}
`,
	})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.errs) == 0 || !strings.Contains(o.errs[0], `duplicate variable "region"`) {
		t.Fatalf("expected duplicate error, got %v", o.errs)
	}
	if !strings.Contains(o.errs[0], "extra.tf") || !strings.Contains(o.errs[0], "main.tf") {
		t.Errorf("error should name both files: %v", o.errs)
	}
	if len(o.moves) != 0 {
		t.Errorf("moves must be aborted on duplicates: %v", o.moves)
	}
}

func TestDuplicateIntoExistingDestinationDetected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.tf": `variable "region" {
  type = string
}
`,
		"variables.tf": `variable "region" {
  type = number
}
`,
	})

	// Single-file targeting: variables.tf is not targeted, but moving
	// main.tf's variable into it would duplicate the address.
	o := processDir(dir, []string{"main.tf"}, defaultCfg())
	if len(o.errs) == 0 || !strings.Contains(o.errs[0], `duplicate variable "region"`) {
		t.Fatalf("expected duplicate error, got %v", o.errs)
	}
	if len(o.moves) != 0 {
		t.Errorf("moves must be aborted: %v", o.moves)
	}
}

func TestRepeatableBlocksAreNotDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"providers.tf": `provider "aws" {
  region = "eu-west-1"
}

provider "aws" {
  alias  = "us"
  region = "us-east-1"
}
`,
		"locals.tf": `locals {
  a = 1
}

locals {
  b = 2
}
`,
		"override.tf": `variable "region" {
  default = "us-east-1"
}
`,
		"variables.tf": `variable "region" {
  type = string
}
`,
	})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.errs) != 0 {
		t.Errorf("providers/locals/override duplication is legal, got errors: %v", o.errs)
	}
}

func TestSortOrdersVariableBlocks(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"variables.tf": `variable "charlie" {
  type = string
}

# Attached to alpha.
variable "alpha" {
  type = string
}

variable "bravo" {
  type = string
}
`})

	cfg := defaultCfg()
	cfg.sort = true
	organize(t, dir, nil, cfg)
	got := readFiles(t, dir)["variables.tf"]

	a := strings.Index(got, `variable "alpha"`)
	b := strings.Index(got, `variable "bravo"`)
	c := strings.Index(got, `variable "charlie"`)
	if !(a < b && b < c) {
		t.Errorf("not sorted:\n%s", got)
	}
	if !strings.Contains(got[:a], "# Attached to alpha.") {
		t.Errorf("attached comment did not travel with sorted block:\n%s", got)
	}

	second := organize(t, dir, nil, cfg)
	if second.changed() {
		t.Errorf("sort is not idempotent: %v", second.writes)
	}
}

func TestSortSkipsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	loose := `variable "zulu" {
  type = string
}

# A standalone comment: sorting would orphan it.

variable "alpha" {
  type = string
}
`
	mixed := `resource "null_resource" "b" {
}

resource "null_resource" "a" {
}
`
	writeFiles(t, dir, map[string]string{"variables.tf": loose, "main.tf": mixed})

	cfg := defaultCfg()
	cfg.sort = true
	organize(t, dir, nil, cfg)
	got := readFiles(t, dir)

	if got["variables.tf"] != loose {
		t.Errorf("file with standalone comment was reordered:\n%s", got["variables.tf"])
	}
	if got["main.tf"] != mixed {
		t.Errorf("resources must never be sorted:\n%s", got["main.tf"])
	}
}

func TestAtomicWritePreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.tf")
	if err := os.WriteFile(path, []byte("variable \"v\" {\ntype=string\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultCfg()
	cfg.fmtOnly = true
	organize(t, dir, nil, cfg)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode not preserved: %v", fi.Mode().Perm())
	}
	if !strings.Contains(readFiles(t, dir)["main.tf"], "type = string") {
		t.Error("content not rewritten")
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

// gitRepo creates a temp git repository and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func inDir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestStagedTfFiles(t *testing.T) {
	repo := gitRepo(t)
	sub := filepath.Join(repo, "modules", "a")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "staged.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(sub, "nested.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "unstaged.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("x"), 0o644)

	cmd := exec.Command("git", "add", "staged.tf", "modules/a/nested.tf", "staged.txt")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	inDir(t, sub) // works from a subdirectory too
	files, errs := stagedTfFiles()
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 staged .tf files, got %v", files)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if base != "staged.tf" && base != "nested.tf" {
			t.Errorf("unexpected file %s", f)
		}
	}
}

func TestInstallHook(t *testing.T) {
	repo := gitRepo(t)
	inDir(t, repo)

	if code := installHook(nil); code != 0 {
		t.Fatalf("install failed with code %d", code)
	}
	path := filepath.Join(repo, ".git", "hooks", "pre-commit")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Error("hook not executable")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "tforg -staged") {
		t.Errorf("hook script wrong:\n%s", b)
	}

	if code := installHook(nil); code != 2 {
		t.Error("second install without -force must refuse")
	}
	if code := installHook([]string{"-force"}); code != 0 {
		t.Error("-force should overwrite")
	}
}
