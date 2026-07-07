package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepoConfig(t *testing.T) {
	rc, err := parseRepoConfig([]byte(`
place "module" "network_data" {
  file = "data.tf"
}

place "module" "legacy" {
  keep = true
}

map {
  terraform = "terraform.tf"
}
`), ".tforg.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.rules) != 2 {
		t.Fatalf("expected 2 rules, got %v", rc.rules)
	}
	if r := rc.rules[0]; r.blockType != "module" || r.pattern != "network_data" || r.file != "data.tf" || r.keep {
		t.Errorf("rule 0 wrong: %+v", r)
	}
	if r := rc.rules[1]; !r.keep || r.file != "" {
		t.Errorf("rule 1 wrong: %+v", r)
	}
	if rc.dest["terraform"] != "terraform.tf" {
		t.Errorf("map override missing: %v", rc.dest)
	}
}

func TestParseRepoConfigErrors(t *testing.T) {
	cases := map[string]string{
		"unknown block":     `weird {}`,
		"one label":         `place "module" { file = "x.tf" }`,
		"both file+keep":    `place "module" "m" { file = "x.tf" keep = true }`,
		"neither":           `place "module" "m" {}`,
		"bad file name":     `place "module" "m" { file = "sub/dir.tf" }`,
		"non-string map":    `map { terraform = true }`,
		"top-level attr":    `file = "x.tf"`,
		"unknown place key": `place "module" "m" { path = "x.tf" }`,
	}
	for name, src := range cases {
		if _, err := parseRepoConfig([]byte(src), ".tforg.hcl"); err == nil {
			t.Errorf("%s: expected error for %q", name, src)
		}
	}
}

func cfgWithRules(rules ...placeRule) config {
	return effectiveConfig(config{}, &repoConfig{rules: rules}, nil, nil)
}

func TestPlaceRuleRoutesModuleToDataTf(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `module "network_data" {
  source = "../modules/network-data"
}

module "vpc" {
  source = "../modules/vpc"
}

resource "null_resource" "r" {
}
`})

	cfg := cfgWithRules(placeRule{blockType: "module", pattern: "network_data", file: "data.tf"})
	organize(t, dir, nil, cfg)
	got := readFiles(t, dir)

	if !strings.Contains(got["data.tf"], `module "network_data"`) {
		t.Errorf("pinned module not in data.tf:\n%s", got["data.tf"])
	}
	if !strings.Contains(got["main.tf"], `module "vpc"`) {
		t.Errorf("unpinned module should stay in main.tf:\n%s", got["main.tf"])
	}
	if strings.Contains(got["main.tf"], "network_data") {
		t.Errorf("pinned module still in main.tf:\n%s", got["main.tf"])
	}

	second := organize(t, dir, nil, cfg)
	if second.changed() {
		t.Errorf("not idempotent with rules: %v", second.writes)
	}
}

func TestKeepRuleLeavesBlockInPlace(t *testing.T) {
	dir := t.TempDir()
	content := `module "legacy" {
  source = "./x"
}
`
	writeFiles(t, dir, map[string]string{"misc.tf": content})

	cfg := cfgWithRules(placeRule{blockType: "module", pattern: "legacy", keep: true})
	o := organize(t, dir, nil, cfg)
	if len(o.moves) != 0 {
		t.Errorf("keep rule ignored: %v", o.moves)
	}
	if got := readFiles(t, dir)["misc.tf"]; got != content {
		t.Errorf("file changed:\n%s", got)
	}
}

func TestGlobPatternStillWorks(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"main.tf": `module "data_accounts" {
  source = "./x"
}

resource "null_resource" "r" {
}
`})

	cfg := cfgWithRules(placeRule{blockType: "module", pattern: "data_*", file: "data.tf"})
	organize(t, dir, nil, cfg)
	if !strings.Contains(readFiles(t, dir)["data.tf"], "data_accounts") {
		t.Error("glob rule did not match")
	}
}

func TestPrecedenceCLIOverConfigOverDefaults(t *testing.T) {
	rc := &repoConfig{
		dest:  map[string]string{"terraform": "terraform.tf"},
		rules: []placeRule{{blockType: "module", pattern: "m", file: "config.tf"}},
	}
	cliDest := map[string]string{"terraform": "settings.tf"}
	cliRules := []placeRule{{blockType: "module", pattern: "m", file: "cli.tf"}}

	cfg := effectiveConfig(config{}, rc, cliDest, cliRules)
	if cfg.dest["terraform"] != "settings.tf" {
		t.Errorf("CLI -map should win over config map: %v", cfg.dest["terraform"])
	}
	if cfg.dest["variable"] != "variables.tf" {
		t.Error("defaults lost")
	}
	if cfg.rules[0].file != "cli.tf" {
		t.Errorf("CLI rules should match first: %+v", cfg.rules)
	}
}

func TestConfigDiscoveryWalksUpward(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "modules", "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, configFileName),
		[]byte(`place "module" "network_data" { file = "data.tf" }`), 0o644)

	loader, err := newConfigLoader(false, "")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := loader.forDir(nested)
	if err != nil {
		t.Fatal(err)
	}
	if rc == nil || len(rc.rules) != 1 {
		t.Fatalf("config not discovered from nested dir: %+v", rc)
	}

	disabled, _ := newConfigLoader(true, "")
	if rc, _ := disabled.forDir(nested); rc != nil {
		t.Error("-no-config should ignore config files")
	}
}

func TestMapFlagPatternSyntax(t *testing.T) {
	dest := map[string]string{}
	var rules []placeRule
	m := &mapFlag{dest: dest, rules: &rules}

	if err := m.Set("terraform=terraform.tf,module:network_data=data.tf"); err != nil {
		t.Fatal(err)
	}
	if dest["terraform"] != "terraform.tf" {
		t.Errorf("plain override lost: %v", dest)
	}
	if len(rules) != 1 || rules[0].pattern != "network_data" || rules[0].file != "data.tf" {
		t.Errorf("pattern rule wrong: %+v", rules)
	}
	for _, bad := range []string{"module:=x.tf", ":m=x.tf", "module:m=dir/x.tf", "nope"} {
		if err := m.Set(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
