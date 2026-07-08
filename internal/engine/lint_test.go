package engine

import (
	"os"
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
	if len(o.Errs) == 0 || !strings.Contains(o.Errs[0], `duplicate variable "region"`) {
		t.Fatalf("expected duplicate error, got %v", o.Errs)
	}
	if !strings.Contains(o.Errs[0], "main.tf:1") || !strings.Contains(o.Errs[0], "extra.tf:1") {
		t.Errorf("error should carry file:line locations: %v", o.Errs)
	}
	if len(o.Moves) != 0 {
		t.Errorf("moves must be aborted on duplicates: %v", o.Moves)
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
	o := ProcessDir(dir, []string{"main.tf"}, defaultCfg())
	if len(o.Errs) == 0 || !strings.Contains(o.Errs[0], `duplicate variable "region"`) {
		t.Fatalf("expected duplicate error, got %v", o.Errs)
	}
	if len(o.Moves) != 0 {
		t.Errorf("moves must be aborted: %v", o.Moves)
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
	if len(o.Errs) != 0 {
		t.Errorf("providers/locals/override duplication is legal, got errors: %v", o.Errs)
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
	if len(o.Errs) != 1 {
		t.Fatalf("expected one duplicate error, got %v", o.Errs)
	}
	e := o.Errs[0]
	if !strings.Contains(e, `duplicate local "team"`) {
		t.Errorf("wrong error: %s", e)
	}
	if !strings.Contains(e, "main.tf:2") || !strings.Contains(e, "locals.tf:3") {
		t.Errorf("error should carry file:line locations: %s", e)
	}
	if len(o.Moves) != 0 {
		t.Error("moves must be aborted on duplicate locals keys")
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
	o := ProcessDir(dir, []string{"main.tf"}, defaultCfg())
	if len(o.Errs) == 0 || !strings.Contains(o.Errs[0], `duplicate local "team"`) {
		t.Fatalf("expected duplicate local error, got %v", o.Errs)
	}
	if !strings.Contains(o.Errs[0], "main.tf:2") || !strings.Contains(o.Errs[0], "locals.tf:2") {
		t.Errorf("expected file:line on both sides: %v", o.Errs)
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
	cfg.Sort = true
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
	if second.Changed() {
		t.Errorf("sort is not idempotent: %v", second.Writes)
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
	cfg.Sort = true
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
	cfg.FmtOnly = true
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
