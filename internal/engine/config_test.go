package engine

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
	if r := rc.rules[0]; r.BlockType != "module" || r.Pattern != "network_data" || r.File != "data.tf" || r.Keep {
		t.Errorf("rule 0 wrong: %+v", r)
	}
	if r := rc.rules[1]; !r.Keep || r.File != "" {
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

func TestParseIgnoreList(t *testing.T) {
	rc, err := parseRepoConfig([]byte(`ignore = ["**/generated/**", "*.gen.tf"]`), ".tforg.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Ignore) != 2 || rc.Ignore[0] != "**/generated/**" {
		t.Errorf("ignore list wrong: %v", rc.Ignore)
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
		if got := MatchesIgnore([]string{c.pattern}, base, filepath.FromSlash(c.file)); got != c.want {
			t.Errorf("pattern %q vs %q: got %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
	if MatchesIgnore(nil, base, "/repo/main.tf") {
		t.Error("empty pattern list must match nothing")
	}
}

func cfgWithRules(rules ...PlaceRule) Config {
	return EffectiveConfig(Config{}, &RepoConfig{rules: rules}, nil, nil)
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

	cfg := cfgWithRules(PlaceRule{BlockType: "module", Pattern: "network_data", File: "data.tf"})
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
	if second.Changed() {
		t.Errorf("not idempotent with rules: %v", second.Writes)
	}
}

func TestKeepRuleLeavesBlockInPlace(t *testing.T) {
	dir := t.TempDir()
	content := `module "legacy" {
  source = "./x"
}
`
	writeFiles(t, dir, map[string]string{"misc.tf": content})

	cfg := cfgWithRules(PlaceRule{BlockType: "module", Pattern: "legacy", Keep: true})
	o := organize(t, dir, nil, cfg)
	if len(o.Moves) != 0 {
		t.Errorf("keep rule ignored: %v", o.Moves)
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

	cfg := cfgWithRules(PlaceRule{BlockType: "module", Pattern: "data_*", File: "data.tf"})
	organize(t, dir, nil, cfg)
	if !strings.Contains(readFiles(t, dir)["data.tf"], "data_accounts") {
		t.Error("glob rule did not match")
	}
}

func TestPrecedenceCLIOverConfigOverDefaults(t *testing.T) {
	rc := &RepoConfig{
		dest:  map[string]string{"terraform": "terraform.tf"},
		rules: []PlaceRule{{BlockType: "module", Pattern: "m", File: "config.tf"}},
	}
	cliDest := map[string]string{"terraform": "settings.tf"}
	cliRules := []PlaceRule{{BlockType: "module", Pattern: "m", File: "cli.tf"}}

	cfg := EffectiveConfig(Config{}, rc, cliDest, cliRules)
	if cfg.Dest["terraform"] != "settings.tf" {
		t.Errorf("CLI -map should win over config map: %v", cfg.Dest["terraform"])
	}
	if cfg.Dest["variable"] != "variables.tf" {
		t.Error("defaults lost")
	}
	if cfg.Rules[0].File != "cli.tf" {
		t.Errorf("CLI rules should match first: %+v", cfg.Rules)
	}
}

func TestConfigDiscoveryWalksUpward(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "modules", "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ConfigFileName),
		[]byte(`place "module" "network_data" { file = "data.tf" }`), 0o644)

	loader, err := NewConfigLoader(false, "")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := loader.ForDir(nested)
	if err != nil {
		t.Fatal(err)
	}
	if rc == nil || len(rc.rules) != 1 {
		t.Fatalf("config not discovered from nested dir: %+v", rc)
	}

	disabled, _ := NewConfigLoader(true, "")
	if rc, _ := disabled.ForDir(nested); rc != nil {
		t.Error("-no-config should ignore config files")
	}
}
