package engine

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// ConfigFileName is the per-repo configuration file discovered upward from
// each target directory.
const ConfigFileName = ".tforg.hcl"

// PlaceRule pins blocks matching a type and name to a destination file (or to
// wherever they currently live, with Keep). Rules run before the type
// mapping; first match wins.
type PlaceRule struct {
	BlockType string
	Pattern   string // matched against the block's labels joined with "."
	File      string
	Keep      bool
}

// RepoConfig is the parsed content of a .tforg.hcl file.
type RepoConfig struct {
	Dir    string   // directory the file was found in; Ignore patterns are relative to it
	Ignore []string // gitignore-style globs for files tforg must never touch
	dest   map[string]string
	rules  []PlaceRule
}

// MatchesIgnore reports whether file matches any ignore pattern. Patterns
// with a slash match the path relative to baseDir (doublestar globs, so
// **/generated/** works); bare patterns match any single path component,
// like .gitignore.
func MatchesIgnore(patterns []string, baseDir, file string) bool {
	if len(patterns) == 0 {
		return false
	}
	target := file
	if baseDir != "" {
		if rel, err := filepath.Rel(baseDir, file); err == nil && !strings.HasPrefix(rel, "..") {
			target = rel
		}
	}
	target = filepath.ToSlash(target)
	for _, pat := range patterns {
		if ok, _ := doublestar.Match(pat, target); ok {
			return true
		}
		if !strings.Contains(pat, "/") {
			for _, seg := range strings.Split(target, "/") {
				if ok, _ := path.Match(pat, seg); ok {
					return true
				}
			}
		}
	}
	return false
}

// destFor resolves where a block belongs: place rules first (first match
// wins), then the type mapping. keep=true means "leave it where it is".
func (c Config) destFor(blk *hclsyntax.Block) (dest string, keep bool) {
	if len(c.Rules) > 0 {
		key := strings.Join(blk.Labels, ".")
		for _, r := range c.Rules {
			if r.BlockType != blk.Type {
				continue
			}
			if ok, err := path.Match(r.Pattern, key); err == nil && ok {
				return r.File, r.Keep
			}
		}
	}
	return c.Dest[blk.Type], false
}

// EffectiveConfig layers destinations and rules for one directory:
// built-in defaults < .tforg.hcl < -map flags.
func EffectiveConfig(base Config, rc *RepoConfig, cliDest map[string]string, cliRules []PlaceRule) Config {
	out := base
	out.Dest = map[string]string{}
	for k, v := range defaultDest {
		out.Dest[k] = v
	}
	if rc != nil {
		for k, v := range rc.dest {
			out.Dest[k] = v
		}
	}
	for k, v := range cliDest {
		out.Dest[k] = v
	}
	out.Rules = append(append([]PlaceRule{}, cliRules...), rulesOf(rc)...)
	return out
}

func rulesOf(rc *RepoConfig) []PlaceRule {
	if rc == nil {
		return nil
	}
	return rc.rules
}

// ValidDestName checks a mapping/rule destination: a bare .tf file name.
func ValidDestName(v string) error {
	if !strings.HasSuffix(v, ".tf") || strings.ContainsAny(v, "/\\") {
		return fmt.Errorf("destination must be a bare .tf file name, got %q", v)
	}
	return nil
}

// ValidPattern checks a place-rule name pattern.
func ValidPattern(p string) error {
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("bad pattern %q: %v", p, err)
	}
	return nil
}

func parseRepoConfig(src []byte, filename string) (*RepoConfig, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}
	body := f.Body.(*hclsyntax.Body)
	rc := &RepoConfig{dest: map[string]string{}}

	for name, attr := range body.Attributes {
		if name != "ignore" {
			return nil, fmt.Errorf("%s: unexpected top-level attribute %q", filename, name)
		}
		v, vdiags := attr.Expr.Value(nil)
		if vdiags.HasErrors() || !v.CanIterateElements() {
			return nil, fmt.Errorf("%s: ignore must be a list of pattern strings", filename)
		}
		for it := v.ElementIterator(); it.Next(); {
			_, ev := it.Element()
			if ev.Type() != cty.String {
				return nil, fmt.Errorf("%s: ignore must be a list of pattern strings", filename)
			}
			pat := ev.AsString()
			if !doublestar.ValidatePattern(pat) {
				return nil, fmt.Errorf("%s: bad ignore pattern %q", filename, pat)
			}
			rc.Ignore = append(rc.Ignore, pat)
		}
	}
	for _, blk := range body.Blocks {
		switch blk.Type {
		case "map":
			if len(blk.Labels) != 0 || len(blk.Body.Blocks) != 0 {
				return nil, fmt.Errorf("%s: map is a flat block of type = \"file.tf\" entries", filename)
			}
			for name, attr := range blk.Body.Attributes {
				v, vdiags := attr.Expr.Value(nil)
				if vdiags.HasErrors() || v.Type() != cty.String {
					return nil, fmt.Errorf("%s: map.%s must be a quoted file name", filename, name)
				}
				if err := ValidDestName(v.AsString()); err != nil {
					return nil, fmt.Errorf("%s: map.%s: %v", filename, name, err)
				}
				rc.dest[name] = v.AsString()
			}
		case "place":
			if len(blk.Labels) != 2 {
				return nil, fmt.Errorf(`%s: place needs two labels: place "TYPE" "NAME" { ... }`, filename)
			}
			rule := PlaceRule{BlockType: blk.Labels[0], Pattern: blk.Labels[1]}
			if err := ValidPattern(rule.Pattern); err != nil {
				return nil, fmt.Errorf("%s: %v", filename, err)
			}
			if len(blk.Body.Blocks) != 0 {
				return nil, fmt.Errorf("%s: place %q %q: no nested blocks allowed", filename, rule.BlockType, rule.Pattern)
			}
			for name, attr := range blk.Body.Attributes {
				v, vdiags := attr.Expr.Value(nil)
				switch name {
				case "file":
					if vdiags.HasErrors() || v.Type() != cty.String {
						return nil, fmt.Errorf("%s: place %q %q: file must be a quoted file name", filename, rule.BlockType, rule.Pattern)
					}
					if err := ValidDestName(v.AsString()); err != nil {
						return nil, fmt.Errorf("%s: place %q %q: %v", filename, rule.BlockType, rule.Pattern, err)
					}
					rule.File = v.AsString()
				case "keep":
					if vdiags.HasErrors() || v.Type() != cty.Bool {
						return nil, fmt.Errorf("%s: place %q %q: keep must be true or false", filename, rule.BlockType, rule.Pattern)
					}
					rule.Keep = v.True()
				default:
					return nil, fmt.Errorf("%s: place %q %q: unknown attribute %q", filename, rule.BlockType, rule.Pattern, name)
				}
			}
			if rule.Keep == (rule.File != "") {
				return nil, fmt.Errorf("%s: place %q %q: set exactly one of file or keep", filename, rule.BlockType, rule.Pattern)
			}
			rc.rules = append(rc.rules, rule)
		default:
			return nil, fmt.Errorf("%s: unknown block %q (expected place or map)", filename, blk.Type)
		}
	}
	return rc, nil
}

// ConfigLoader finds the nearest .tforg.hcl for a directory, walking upward
// (like .gitignore discovery), with per-directory memoization.
type ConfigLoader struct {
	disabled bool
	explicit *RepoConfig
	cache    map[string]*RepoConfig
}

func NewConfigLoader(disabled bool, explicitPath string) (*ConfigLoader, error) {
	l := &ConfigLoader{disabled: disabled, cache: map[string]*RepoConfig{}}
	if explicitPath != "" && !disabled {
		b, err := os.ReadFile(explicitPath)
		if err != nil {
			return nil, err
		}
		rc, err := parseRepoConfig(b, explicitPath)
		if err != nil {
			return nil, err
		}
		if abs, err := filepath.Abs(explicitPath); err == nil {
			rc.Dir = filepath.Dir(abs)
		}
		l.explicit = rc
	}
	return l, nil
}

func (l *ConfigLoader) ForDir(dir string) (*RepoConfig, error) {
	if l.disabled {
		return nil, nil
	}
	if l.explicit != nil {
		return l.explicit, nil
	}
	if rc, ok := l.cache[dir]; ok {
		return rc, nil
	}
	var rc *RepoConfig
	cfgPath := filepath.Join(dir, ConfigFileName)
	if b, err := os.ReadFile(cfgPath); err == nil {
		parsed, perr := parseRepoConfig(b, cfgPath)
		if perr != nil {
			return nil, perr
		}
		parsed.Dir = dir
		rc = parsed
	} else if parent := filepath.Dir(dir); parent != dir {
		parentRC, perr := l.ForDir(parent)
		if perr != nil {
			return nil, perr
		}
		rc = parentRC
	}
	l.cache[dir] = rc
	return rc, nil
}
