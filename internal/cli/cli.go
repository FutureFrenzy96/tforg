// Package cli parses flags, walks the target tree, and reports results; the
// actual file rewriting lives in internal/engine.
package cli

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/FutureFrenzy96/tforg/internal/engine"
	"github.com/FutureFrenzy96/tforg/internal/gitx"
	"github.com/FutureFrenzy96/tforg/internal/ui"
)

const usageText = `tforg — fast Terraform formatter + file organizer

Formats .tf files (identical output to terraform fmt) and moves top-level
blocks into their conventional files within each directory:

  resource, module -> main.tf        variable -> variables.tf
  data             -> data.tf        output   -> outputs.tf
  locals           -> locals.tf      provider -> providers.tf
  terraform        -> versions.tf    moved    -> moved.tf
  import           -> imports.tf     removed  -> removed.tf
  check            -> checks.tf      ephemeral -> ephemeral.tf

Destination files are created as needed; source files left empty are deleted.
Blocks only move between files in the same directory.

Usage:
  tforg [flags] [path ...]     paths are directories (recursive) or .tf files;
                               defaults to the current directory
  tforg install-hook           write a .git/hooks/pre-commit that runs
                               'tforg -staged' (-force overwrites an existing hook)

Flags:
  -staged          target the .tf files currently staged in git
  -check           report what would change, write nothing; exit 1 if dirty
  -diff            show unified diffs of pending changes (implies -check)
  -sort            alphabetize variable/output blocks within their files
  -fmt-only        format only; do not move blocks between files
  -map type=file   override a type's destination, or pin one block with
                   type:name=file (repeatable, comma-separated), e.g.
                   -map terraform=terraform.tf,module:network_data=data.tf
  -exclude glob    skip matching files, e.g. -exclude '**/generated/**'
                   (repeatable, comma-separated; relative to the working dir)
  -config path     use this config file instead of discovering .tforg.hcl
  -no-config       ignore .tforg.hcl files
  -no-color        disable colored output (NO_COLOR and CLICOLOR_FORCE are
                   also honored)
  -quiet           suppress non-error output
  -version         print version

Config:
  A .tforg.hcl in the target directory or any parent applies placement rules
  ahead of the type mapping (first match wins; -map flags win over the file):
    place "module" "network_data" { file = "data.tf" }   # pin to a file
    place "module" "legacy" { keep = true }              # leave where it is
    map { terraform = "terraform.tf" }                   # change type defaults
    ignore = ["**/generated/**", "*.gen.tf"]             # never touch these

Exit codes: 0 nothing to do · 1 changes made (or needed with -check) · 2 error
`

type excludeFlag struct{ vals *[]string }

func (e *excludeFlag) String() string { return "" }

func (e *excludeFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !doublestar.ValidatePattern(p) {
			return fmt.Errorf("bad exclude pattern %q", p)
		}
		*e.vals = append(*e.vals, p)
	}
	return nil
}

type mapFlag struct {
	dest  map[string]string
	rules *[]engine.PlaceRule
}

func (m *mapFlag) String() string { return "" }

func (m *mapFlag) Set(v string) error {
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" || val == "" {
			return fmt.Errorf("expected type=file.tf or type:name=file.tf, got %q", pair)
		}
		if err := engine.ValidDestName(val); err != nil {
			return err
		}
		if typ, pattern, hasPattern := strings.Cut(k, ":"); hasPattern {
			if typ == "" || pattern == "" {
				return fmt.Errorf("expected type:name=file.tf, got %q", pair)
			}
			if err := engine.ValidPattern(pattern); err != nil {
				return err
			}
			*m.rules = append(*m.rules, engine.PlaceRule{BlockType: typ, Pattern: pattern, File: val})
		} else {
			m.dest[k] = val
		}
	}
	return nil
}

// Run executes the tforg command line and returns its exit code. version is
// the release version stamped into the binary, or empty for dev builds.
func Run(args []string, version string) int {
	start := time.Now()
	if len(args) > 0 && args[0] == "install-hook" {
		return gitx.InstallHook(args[1:])
	}

	cfg := engine.Config{}
	cliDest := map[string]string{}
	var cliRules []engine.PlaceRule
	var cliExcludes []string

	fl := flag.NewFlagSet("tforg", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	noColor := fl.Bool("no-color", false, "")
	staged := fl.Bool("staged", false, "")
	showVersion := fl.Bool("version", false, "")
	configPath := fl.String("config", "", "")
	noConfig := fl.Bool("no-config", false, "")
	fl.BoolVar(&cfg.Check, "check", false, "")
	fl.BoolVar(&cfg.Diff, "diff", false, "")
	fl.BoolVar(&cfg.Sort, "sort", false, "")
	fl.BoolVar(&cfg.Quiet, "quiet", false, "")
	fl.BoolVar(&cfg.FmtOnly, "fmt-only", false, "")
	fl.Var(&mapFlag{dest: cliDest, rules: &cliRules}, "map", "")
	fl.Var(&excludeFlag{vals: &cliExcludes}, "exclude", "")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	pal := ui.NewPalette(*noColor)
	if *showVersion {
		fmt.Println("tforg", versionString(version))
		return 0
	}
	if cfg.Diff {
		cfg.Check = true
	}

	paths := fl.Args()
	if *staged {
		cfg.Staged = true
		if len(paths) > 0 {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), "-staged cannot be combined with explicit paths")
			return 2
		}
		var errs []string
		paths, errs = gitx.StagedTfFiles()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), e)
		}
		if len(errs) > 0 {
			return 2
		}
		if len(paths) == 0 {
			if !cfg.Quiet {
				fmt.Println(pal.Dim("✓ no staged .tf files"))
			}
			return 0
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}

	targets, errs := collectTargets(paths)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, pal.Red("✗"), e)
	}
	if len(errs) > 0 {
		return 2
	}

	// Resolve each directory's .tforg.hcl (nearest ancestor wins) serially,
	// so the parallel workers below only read their own config value.
	loader, err := engine.NewConfigLoader(*noConfig, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, pal.Red("✗"), err)
		return 2
	}

	// Drop files matched by -exclude patterns (relative to the working
	// directory) or by the nearest config's ignore list (relative to the
	// config file) — generated Terraform must never be rewritten.
	cwd, _ := os.Getwd()
	excluded := 0
	for dir, bases := range targets {
		rc, err := loader.ForDir(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), err)
			return 2
		}
		var kept []string
		for _, base := range bases {
			full := filepath.Join(dir, base)
			if engine.MatchesIgnore(cliExcludes, cwd, full) {
				excluded++
				continue
			}
			if rc != nil && engine.MatchesIgnore(rc.Ignore, rc.Dir, full) {
				excluded++
				continue
			}
			kept = append(kept, base)
		}
		if len(kept) == 0 {
			delete(targets, dir)
		} else {
			targets[dir] = kept
		}
	}

	totalFiles := 0
	for _, bases := range targets {
		totalFiles += len(bases)
	}

	// An empty scope is more often a mistyped path than a clean tree; say so
	// instead of reporting success over nothing.
	if totalFiles == 0 {
		if !cfg.Quiet {
			if excluded > 0 {
				fmt.Println(pal.Yellow(fmt.Sprintf("! no .tf files to process — %s excluded by ignore/exclude patterns", plural(excluded, "file", "files"))))
			} else {
				fmt.Println(pal.Yellow("! no .tf files found under " + displayPaths(paths)))
			}
		}
		return 0
	}

	dirs := make([]string, 0, len(targets))
	for d := range targets {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	dirCfgs := make([]engine.Config, len(dirs))
	for i, dir := range dirs {
		rc, err := loader.ForDir(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), err)
			return 2
		}
		dirCfgs[i] = engine.EffectiveConfig(cfg, rc, cliDest, cliRules)
	}

	// Each directory is an independent Terraform module; process them in
	// parallel and write results from the worker as well.
	outcomes := make([]engine.DirOutcome, len(dirs))
	applyErrs := make([][]string, len(dirs))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, dir string) {
			defer wg.Done()
			defer func() { <-sem }()
			outcomes[i] = engine.ProcessDir(dir, targets[dir], dirCfgs[i])
			if !cfg.Check && len(outcomes[i].Errs) == 0 {
				applyErrs[i] = engine.Apply(outcomes[i])
			}
		}(i, dir)
	}
	wg.Wait()

	return report(outcomes, applyErrs, cfg, pal, totalFiles, time.Since(start))
}

func report(outcomes []engine.DirOutcome, applyErrs [][]string, cfg engine.Config, pal ui.Palette, totalFiles int, elapsed time.Duration) int {
	cwd, _ := os.Getwd()
	relDir := func(dir string) string {
		if r, err := filepath.Rel(cwd, dir); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return dir
	}
	verb := func(past, cond string) string {
		if cfg.Check {
			return cond
		}
		return past
	}

	nErrs, changedFiles, changedDirs := 0, 0, 0
	for i, o := range outcomes {
		for _, e := range o.Errs {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), e)
			nErrs++
		}
		for _, e := range applyErrs[i] {
			fmt.Fprintln(os.Stderr, pal.Red("✗"), e)
			nErrs++
		}
		if len(o.Errs) > 0 || !o.Changed() {
			continue
		}
		changedDirs++
		changedFiles += len(o.Writes) + len(o.Deletes)
		if cfg.Quiet {
			continue
		}

		fmt.Println(pal.Bold(relDir(o.Dir)))
		width := 0
		for _, m := range o.Moves {
			if n := len(m.From) + len(m.Dest); n > width {
				width = n
			}
		}
		for _, m := range o.Moves {
			pad := strings.Repeat(" ", width-len(m.From)-len(m.Dest))
			fmt.Printf("  %s %s %s%s  %s\n",
				pal.File(m.From), pal.Dim("→"), pal.File(m.Dest), pad, pal.Dim(m.Desc))
		}
		for _, base := range sortedKeys(o.Creates) {
			fmt.Printf("  %s %s  %s\n", pal.Green("+"), pal.File(base), pal.Green(verb("created", "would create")))
		}
		for _, base := range sortedKeys(o.Deletes) {
			fmt.Printf("  %s %s  %s\n", pal.Red("-"), pal.File(base), pal.Red(verb("deleted (empty)", "would delete (empty)")))
		}
		for _, base := range o.FmtOnly {
			fmt.Printf("  %s %s  %s\n", pal.Dim("~"), pal.File(base), pal.Dim(verb("reformatted", "needs reformatting")))
		}
		if cfg.Diff {
			for _, base := range sortedKeys(o.Writes) {
				ui.PrintDiff(pal, filepath.Join(relDir(o.Dir), base), o.Origs[base], o.Writes[base])
			}
			for _, base := range sortedKeys(o.Deletes) {
				ui.PrintDiff(pal, filepath.Join(relDir(o.Dir), base), o.Origs[base], nil)
			}
		}
		fmt.Println()
	}

	dur := elapsed.Round(time.Millisecond)
	if dur == 0 {
		dur = elapsed.Round(time.Microsecond)
	}
	summary := func(s string) {
		if !cfg.Quiet {
			fmt.Println(s)
		}
	}

	switch {
	case nErrs > 0:
		fmt.Fprintln(os.Stderr, pal.Red(fmt.Sprintf("✗ %s", plural(nErrs, "error", "errors"))))
		return 2
	case changedFiles > 0 && cfg.Check:
		// changedFiles can exceed totalFiles (created files are not part of
		// the checked scope), so report the two counts as separate facts.
		summary(pal.Yellow(fmt.Sprintf("✗ %s need changes", plural(changedFiles, "file", "files"))) +
			" " + pal.Dim(fmt.Sprintf("· checked %s in %s · run tforg to apply",
			plural(totalFiles, "file", "files"), plural(len(outcomes), "directory", "directories"))))
		return 1
	case changedFiles > 0:
		summary(pal.Green(fmt.Sprintf("✓ fixed %s in %s", plural(changedFiles, "file", "files"), plural(changedDirs, "directory", "directories"))) +
			" " + pal.Dim(fmt.Sprintf("· %s", dur)))
		if cfg.Staged {
			summary(pal.Yellow("! commit aborted so you can review — stage the fixed files and commit again"))
		}
		return 1
	default:
		summary(pal.Dim(fmt.Sprintf("✓ nothing to do · %s clean · %s", plural(totalFiles, "file", "files"), dur)))
		return 0
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// displayPaths keeps the "no files found" message readable when many paths
// were given (e.g. a long -staged list).
func displayPaths(paths []string) string {
	if len(paths) <= 3 {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, … (%d paths)", strings.Join(paths[:3], ", "), len(paths))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// collectTargets expands the given paths into a map of directory ->
// sorted .tf base names. Directories are walked recursively, skipping
// VCS/vendor internals and hidden directories.
func collectTargets(paths []string) (map[string][]string, []string) {
	found := map[string]map[string]bool{}
	var errs []string

	addFile := func(p string) {
		abs, err := filepath.Abs(p)
		if err != nil {
			errs = append(errs, err.Error())
			return
		}
		dir, base := filepath.Split(abs)
		dir = filepath.Clean(dir)
		if found[dir] == nil {
			found[dir] = map[string]bool{}
		}
		found[dir][base] = true
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if !info.IsDir() {
			if strings.HasSuffix(p, ".tf") {
				addFile(p)
			} else {
				errs = append(errs, fmt.Sprintf("%s: not a .tf file", p))
			}
			continue
		}
		root := p
		walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != root && (name == ".terraform" || name == ".git" || name == "node_modules" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".tf") {
				addFile(path)
			}
			return nil
		})
		if walkErr != nil {
			errs = append(errs, walkErr.Error())
		}
	}

	targets := make(map[string][]string, len(found))
	for dir, set := range found {
		bases := make([]string, 0, len(set))
		for b := range set {
			bases = append(bases, b)
		}
		sort.Strings(bases)
		targets[dir] = bases
	}
	return targets, errs
}
