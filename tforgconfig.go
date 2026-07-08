package main

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

const configFileName = ".tforg.hcl"

// placeRule pins blocks matching a type and name to a destination file (or to
// wherever they currently live, with keep). Rules run before the type mapping;
// first match wins.
type placeRule struct {
	blockType string
	pattern   string // matched against the block's labels joined with "."
	file      string
	keep      bool
}

// repoConfig is the parsed content of a .tforg.hcl file.
type repoConfig struct {
	dir    string            // directory the file was found in; ignore patterns are relative to it
	dest   map[string]string // overrides of the built-in type mapping
	rules  []placeRule
	ignore []string
}

// matchesIgnore reports whether file matches any ignore pattern. Patterns
// with a slash match the path relative to baseDir (doublestar globs, so
// **/generated/** works); bare patterns match any single path component,
// like .gitignore.
func matchesIgnore(patterns []string, baseDir, file string) bool {
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
func (c config) destFor(blk *hclsyntax.Block) (dest string, keep bool) {
	if len(c.rules) > 0 {
		key := strings.Join(blk.Labels, ".")
		for _, r := range c.rules {
			if r.blockType != blk.Type {
				continue
			}
			if ok, err := path.Match(r.pattern, key); err == nil && ok {
				return r.file, r.keep
			}
		}
	}
	return c.dest[blk.Type], false
}

// effectiveConfig layers destinations and rules for one directory:
// built-in defaults < .tforg.hcl < -map flags.
func effectiveConfig(base config, rc *repoConfig, cliDest map[string]string, cliRules []placeRule) config {
	out := base
	out.dest = map[string]string{}
	for k, v := range defaultDest {
		out.dest[k] = v
	}
	if rc != nil {
		for k, v := range rc.dest {
			out.dest[k] = v
		}
	}
	for k, v := range cliDest {
		out.dest[k] = v
	}
	out.rules = append(append([]placeRule{}, cliRules...), rulesOf(rc)...)
	return out
}

func rulesOf(rc *repoConfig) []placeRule {
	if rc == nil {
		return nil
	}
	return rc.rules
}

func validDestName(v string) error {
	if !strings.HasSuffix(v, ".tf") || strings.ContainsAny(v, "/\\") {
		return fmt.Errorf("destination must be a bare .tf file name, got %q", v)
	}
	return nil
}

func validPattern(p string) error {
	if _, err := path.Match(p, ""); err != nil {
		return fmt.Errorf("bad pattern %q: %v", p, err)
	}
	return nil
}

func parseRepoConfig(src []byte, filename string) (*repoConfig, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}
	body := f.Body.(*hclsyntax.Body)
	rc := &repoConfig{dest: map[string]string{}}

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
			rc.ignore = append(rc.ignore, pat)
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
				if err := validDestName(v.AsString()); err != nil {
					return nil, fmt.Errorf("%s: map.%s: %v", filename, name, err)
				}
				rc.dest[name] = v.AsString()
			}
		case "place":
			if len(blk.Labels) != 2 {
				return nil, fmt.Errorf(`%s: place needs two labels: place "TYPE" "NAME" { ... }`, filename)
			}
			rule := placeRule{blockType: blk.Labels[0], pattern: blk.Labels[1]}
			if err := validPattern(rule.pattern); err != nil {
				return nil, fmt.Errorf("%s: %v", filename, err)
			}
			if len(blk.Body.Blocks) != 0 {
				return nil, fmt.Errorf("%s: place %q %q: no nested blocks allowed", filename, rule.blockType, rule.pattern)
			}
			for name, attr := range blk.Body.Attributes {
				v, vdiags := attr.Expr.Value(nil)
				switch name {
				case "file":
					if vdiags.HasErrors() || v.Type() != cty.String {
						return nil, fmt.Errorf("%s: place %q %q: file must be a quoted file name", filename, rule.blockType, rule.pattern)
					}
					if err := validDestName(v.AsString()); err != nil {
						return nil, fmt.Errorf("%s: place %q %q: %v", filename, rule.blockType, rule.pattern, err)
					}
					rule.file = v.AsString()
				case "keep":
					if vdiags.HasErrors() || v.Type() != cty.Bool {
						return nil, fmt.Errorf("%s: place %q %q: keep must be true or false", filename, rule.blockType, rule.pattern)
					}
					rule.keep = v.True()
				default:
					return nil, fmt.Errorf("%s: place %q %q: unknown attribute %q", filename, rule.blockType, rule.pattern, name)
				}
			}
			if rule.keep == (rule.file != "") {
				return nil, fmt.Errorf("%s: place %q %q: set exactly one of file or keep", filename, rule.blockType, rule.pattern)
			}
			rc.rules = append(rc.rules, rule)
		default:
			return nil, fmt.Errorf("%s: unknown block %q (expected place or map)", filename, blk.Type)
		}
	}
	return rc, nil
}

// configLoader finds the nearest .tforg.hcl for a directory, walking upward
// (like .gitignore discovery), with per-directory memoization.
type configLoader struct {
	disabled bool
	explicit *repoConfig
	cache    map[string]*repoConfig
}

func newConfigLoader(disabled bool, explicitPath string) (*configLoader, error) {
	l := &configLoader{disabled: disabled, cache: map[string]*repoConfig{}}
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
			rc.dir = filepath.Dir(abs)
		}
		l.explicit = rc
	}
	return l, nil
}

func (l *configLoader) forDir(dir string) (*repoConfig, error) {
	if l.disabled {
		return nil, nil
	}
	if l.explicit != nil {
		return l.explicit, nil
	}
	if rc, ok := l.cache[dir]; ok {
		return rc, nil
	}
	var rc *repoConfig
	cfgPath := filepath.Join(dir, configFileName)
	if b, err := os.ReadFile(cfgPath); err == nil {
		parsed, perr := parseRepoConfig(b, cfgPath)
		if perr != nil {
			return nil, perr
		}
		parsed.dir = dir
		rc = parsed
	} else if parent := filepath.Dir(dir); parent != dir {
		parentRC, perr := l.forDir(parent)
		if perr != nil {
			return nil, perr
		}
		rc = parentRC
	}
	l.cache[dir] = rc
	return rc, nil
}
