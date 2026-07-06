package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func defaultCfg() config { return config{dest: defaultDest} }

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// organize runs processDir over the given targets (all .tf files when nil)
// and applies the result, mirroring what main does.
func organize(t *testing.T, dir string, targets []string, cfg config) dirOutcome {
	t.Helper()
	if targets == nil {
		for name := range readFiles(t, dir) {
			targets = append(targets, name)
		}
		sort.Strings(targets)
	}
	o := processDir(dir, targets, cfg)
	if len(o.errs) == 0 {
		if errs := applyOutcome(o); len(errs) > 0 {
			t.Fatalf("apply failed: %v", errs)
		}
	}
	return o
}

func TestSplitsBlocksIntoConventionalFiles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `terraform {
  required_version = ">= 1.5"
}

provider "aws" {
  region = "eu-west-1"
}

variable "name" {
  type = string
}

data "aws_ami" "ubuntu" {
  most_recent = true
}

locals {
  tag = "x"
}

output "id" {
  value = data.aws_ami.ubuntu.id
}

resource "aws_instance" "web" {
  ami = data.aws_ami.ubuntu.id
}
`})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)

	want := map[string]string{
		"versions.tf":  "terraform {",
		"providers.tf": `provider "aws"`,
		"variables.tf": `variable "name"`,
		"data.tf":      `data "aws_ami"`,
		"locals.tf":    "locals {",
		"outputs.tf":   `output "id"`,
		"main.tf":      `resource "aws_instance"`,
	}
	for file, marker := range want {
		if !strings.Contains(got[file], marker) {
			t.Errorf("%s missing %q; content:\n%s", file, marker, got[file])
		}
	}
	for _, absent := range []string{"variable ", "data ", "provider ", "terraform {", "locals {", "output "} {
		if strings.Contains(got["main.tf"], absent) {
			t.Errorf("main.tf still contains %q:\n%s", absent, got["main.tf"])
		}
	}
}

func TestLeadingCommentsTravelWithBlock(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `# The instance AMI.
# Ubuntu 24.04.
data "aws_ami" "u" {
  most_recent = true
}

# Standalone comment, separated by a blank line: stays put.

variable "x" {
  type = string
}

resource "null_resource" "keep" {
} # trailing comment rides along
`})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)

	if !strings.HasPrefix(got["data.tf"], "# The instance AMI.\n# Ubuntu 24.04.\n") {
		t.Errorf("data.tf lost leading comments:\n%s", got["data.tf"])
	}
	if strings.Contains(got["variables.tf"], "Standalone") {
		t.Errorf("blank-line-separated comment should not travel:\n%s", got["variables.tf"])
	}
	if !strings.Contains(got["main.tf"], "Standalone") {
		t.Errorf("standalone comment lost from main.tf:\n%s", got["main.tf"])
	}
	if !strings.Contains(got["main.tf"], "# trailing comment rides along") {
		t.Errorf("trailing same-line comment lost:\n%s", got["main.tf"])
	}
}

func TestHeredocBodiesAreOpaque(t *testing.T) {
	dir := t.TempDir()
	heredoc := "# not a comment\n\nvariable \"fake\" {\n}\n"
	writeFiles(t, dir, map[string]string{"main.tf": `variable "x" {
  type = string
}

resource "aws_s3_bucket_policy" "p" {
  policy = <<EOT
` + heredoc + `EOT
}
`})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)

	if !strings.Contains(got["main.tf"], heredoc) {
		t.Errorf("heredoc body altered:\n%s", got["main.tf"])
	}
	if strings.Contains(got["variables.tf"], "fake") {
		t.Errorf("fake block inside heredoc was moved:\n%s", got["variables.tf"])
	}
	if !strings.Contains(got["variables.tf"], `variable "x"`) {
		t.Errorf("real variable not moved:\n%s", got["variables.tf"])
	}
}

func TestInterpolationWithNestedBraces(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `locals {
  v = "${var.a}-${jsonencode({ b = "}" })}"
}

resource "null_resource" "r" {
}
`})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)
	if !strings.Contains(got["locals.tf"], `jsonencode({ b = "}" })`) {
		t.Errorf("interpolation mangled:\n%s", got["locals.tf"])
	}
}

func TestIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `variable "a" {
  type = string
}

resource "null_resource" "r" {
}

output "o" {
  value = 1
}
`})

	organize(t, dir, nil, defaultCfg())
	second := organize(t, dir, nil, defaultCfg())
	if second.changed() {
		t.Errorf("second run not a no-op: writes=%v deletes=%v", second.writes, second.deletes)
	}
}

func TestOverrideFilesAreNotReorganized(t *testing.T) {
	dir := t.TempDir()
	content := `variable "x" {
  type = string
}
`
	writeFiles(t, dir, map[string]string{
		"override.tf":      content,
		"prod_override.tf": content,
	})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.moves) != 0 {
		t.Errorf("blocks moved out of override files: %v", o.moves)
	}
	got := readFiles(t, dir)
	if got["override.tf"] != content || got["prod_override.tf"] != content {
		t.Error("override file content changed")
	}
}

func TestEmptiedSourceIsDeleted(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"misc.tf": `variable "x" {
  type = string
}
`})

	o := organize(t, dir, nil, defaultCfg())
	if !o.deletes["misc.tf"] {
		t.Errorf("expected misc.tf to be deleted; deletes=%v", o.deletes)
	}
	if _, err := os.Stat(filepath.Join(dir, "misc.tf")); !os.IsNotExist(err) {
		t.Error("misc.tf still exists")
	}
}

func TestAppendsToExistingDestination(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"variables.tf": `variable "a" {
  type = string
}
`,
		"main.tf": `variable "b" {
  type = number
}

resource "null_resource" "r" {
}
`,
	})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)
	ai := strings.Index(got["variables.tf"], `variable "a"`)
	bi := strings.Index(got["variables.tf"], `variable "b"`)
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("variables.tf wrong: existing content must precede appended:\n%s", got["variables.tf"])
	}
}

func TestFormatsLikeTerraformFmt(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": "resource \"null_resource\" \"r\" {\ncount=1\ntriggers={a=\"b\"}\n}\n"})

	o := organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)
	if !strings.Contains(got["main.tf"], "  count    = 1") {
		t.Errorf("not formatted:\n%s", got["main.tf"])
	}
	if len(o.fmtOnly) != 1 || o.fmtOnly[0] != "main.tf" {
		t.Errorf("expected fmtOnly=[main.tf], got %v", o.fmtOnly)
	}
}

func TestParseErrorLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	broken := "resource \"x\" \"y\" {\n  oops\n"
	writeFiles(t, dir, map[string]string{"main.tf": broken})

	o := organize(t, dir, nil, defaultCfg())
	if len(o.errs) == 0 {
		t.Fatal("expected parse errors")
	}
	got := readFiles(t, dir)
	if got["main.tf"] != broken {
		t.Errorf("broken file was modified:\n%s", got["main.tf"])
	}
}

func TestSingleFileTargeting(t *testing.T) {
	dir := t.TempDir()
	untouched := "variable \"other\" {\ntype=string\n}\n" // deliberately misformatted
	writeFiles(t, dir, map[string]string{
		"main.tf":  "variable \"v\" {\n  type = string\n}\n\nresource \"null_resource\" \"r\" {\n}\n",
		"extra.tf": untouched,
	})

	organize(t, dir, []string{"main.tf"}, defaultCfg())
	got := readFiles(t, dir)
	if got["extra.tf"] != untouched {
		t.Errorf("untargeted file was modified:\n%s", got["extra.tf"])
	}
	if !strings.Contains(got["variables.tf"], `variable "v"`) {
		t.Errorf("targeted file's variable not moved:\n%s", got["variables.tf"])
	}
}

func TestUnknownBlockTypeStays(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `widget "w" {
}

data "aws_ami" "u" {
  most_recent = true
}
`})

	organize(t, dir, nil, defaultCfg())
	got := readFiles(t, dir)
	if !strings.Contains(got["main.tf"], `widget "w"`) {
		t.Errorf("unknown block type moved:\n%s", got["main.tf"])
	}
	if !strings.Contains(got["data.tf"], `data "aws_ami"`) {
		t.Errorf("data block not moved:\n%s", got["data.tf"])
	}
}

func TestMapOverride(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `terraform {
  required_version = ">= 1.5"
}

resource "null_resource" "r" {
}
`})

	cfg := defaultCfg()
	cfg.dest = map[string]string{}
	for k, v := range defaultDest {
		cfg.dest[k] = v
	}
	cfg.dest["terraform"] = "terraform.tf"

	organize(t, dir, nil, cfg)
	got := readFiles(t, dir)
	if !strings.Contains(got["terraform.tf"], "terraform {") {
		t.Errorf("map override ignored; files: %v", got)
	}
}

func TestFmtOnlyMode(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": "variable \"v\" {\ntype=string\n}\n"})

	cfg := defaultCfg()
	cfg.fmtOnly = true
	o := organize(t, dir, nil, cfg)
	if len(o.moves) != 0 {
		t.Errorf("fmt-only mode moved blocks: %v", o.moves)
	}
	got := readFiles(t, dir)
	if !strings.Contains(got["main.tf"], "type = string") {
		t.Errorf("fmt-only mode did not format:\n%s", got["main.tf"])
	}
}

func TestCheckModeWritesNothing(t *testing.T) {
	dir := t.TempDir()
	orig := "variable \"v\" {\ntype=string\n}\n\nresource \"null_resource\" \"r\" {\n}\n"
	writeFiles(t, dir, map[string]string{"main.tf": orig})

	cfg := defaultCfg()
	cfg.check = true
	o := processDir(dir, []string{"main.tf"}, cfg) // no apply, as in main
	if !o.changed() {
		t.Error("check mode should report pending changes")
	}
	got := readFiles(t, dir)
	if got["main.tf"] != orig || len(got) != 1 {
		t.Errorf("check mode touched the tree: %v", got)
	}
}
