package main

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

Exit codes: 0 nothing to do · 1 changes made (or needed with -check) · 2 error
`

func main() {
	os.Exit(run(os.Args[1:]))
}

type mapFlag struct {
	dest  map[string]string
	rules *[]placeRule
}

func (m *mapFlag) String() string { return "" }

func (m *mapFlag) Set(v string) error {
	for _, pair := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" || val == "" {
			return fmt.Errorf("expected type=file.tf or type:name=file.tf, got %q", pair)
		}
		if err := validDestName(val); err != nil {
			return err
		}
		if typ, pattern, hasPattern := strings.Cut(k, ":"); hasPattern {
			if typ == "" || pattern == "" {
				return fmt.Errorf("expected type:name=file.tf, got %q", pair)
			}
			if err := validPattern(pattern); err != nil {
				return err
			}
			*m.rules = append(*m.rules, placeRule{blockType: typ, pattern: pattern, file: val})
		} else {
			m.dest[k] = val
		}
	}
	return nil
}

func run(args []string) int {
	start := time.Now()
	if len(args) > 0 && args[0] == "install-hook" {
		return installHook(args[1:])
	}

	cfg := config{}
	cliDest := map[string]string{}
	var cliRules []placeRule

	fl := flag.NewFlagSet("tforg", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	noColor := fl.Bool("no-color", false, "")
	staged := fl.Bool("staged", false, "")
	showVersion := fl.Bool("version", false, "")
	configPath := fl.String("config", "", "")
	noConfig := fl.Bool("no-config", false, "")
	fl.BoolVar(&cfg.check, "check", false, "")
	fl.BoolVar(&cfg.diff, "diff", false, "")
	fl.BoolVar(&cfg.sort, "sort", false, "")
	fl.BoolVar(&cfg.quiet, "quiet", false, "")
	fl.BoolVar(&cfg.fmtOnly, "fmt-only", false, "")
	fl.Var(&mapFlag{dest: cliDest, rules: &cliRules}, "map", "")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	pal := newPalette(*noColor)
	if *showVersion {
		fmt.Println("tforg", versionString())
		return 0
	}
	if cfg.diff {
		cfg.check = true
	}

	paths := fl.Args()
	if *staged {
		cfg.staged = true
		if len(paths) > 0 {
			fmt.Fprintln(os.Stderr, pal.red("✗"), "-staged cannot be combined with explicit paths")
			return 2
		}
		var errs []string
		paths, errs = stagedTfFiles()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, pal.red("✗"), e)
		}
		if len(errs) > 0 {
			return 2
		}
		if len(paths) == 0 {
			if !cfg.quiet {
				fmt.Println(pal.dim("✓ no staged .tf files"))
			}
			return 0
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}

	targets, errs := collectTargets(paths)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, pal.red("✗"), e)
	}
	if len(errs) > 0 {
		return 2
	}
	totalFiles := 0
	for _, bases := range targets {
		totalFiles += len(bases)
	}

	dirs := make([]string, 0, len(targets))
	for d := range targets {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	// Resolve each directory's .tforg.hcl (nearest ancestor wins) serially,
	// so the parallel workers below only read their own config value.
	loader, err := newConfigLoader(*noConfig, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, pal.red("✗"), err)
		return 2
	}
	dirCfgs := make([]config, len(dirs))
	for i, dir := range dirs {
		rc, err := loader.forDir(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, pal.red("✗"), err)
			return 2
		}
		dirCfgs[i] = effectiveConfig(cfg, rc, cliDest, cliRules)
	}

	// Each directory is an independent Terraform module; process them in
	// parallel and write results from the worker as well.
	outcomes := make([]dirOutcome, len(dirs))
	applyErrs := make([][]string, len(dirs))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, dir string) {
			defer wg.Done()
			defer func() { <-sem }()
			outcomes[i] = processDir(dir, targets[dir], dirCfgs[i])
			if !cfg.check && len(outcomes[i].errs) == 0 {
				applyErrs[i] = applyOutcome(outcomes[i])
			}
		}(i, dir)
	}
	wg.Wait()

	return report(outcomes, applyErrs, cfg, pal, totalFiles, time.Since(start))
}

func report(outcomes []dirOutcome, applyErrs [][]string, cfg config, pal palette, totalFiles int, elapsed time.Duration) int {
	cwd, _ := os.Getwd()
	relDir := func(dir string) string {
		if r, err := filepath.Rel(cwd, dir); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return dir
	}
	verb := func(past, cond string) string {
		if cfg.check {
			return cond
		}
		return past
	}

	nErrs, changedFiles, changedDirs := 0, 0, 0
	for i, o := range outcomes {
		for _, e := range o.errs {
			fmt.Fprintln(os.Stderr, pal.red("✗"), e)
			nErrs++
		}
		for _, e := range applyErrs[i] {
			fmt.Fprintln(os.Stderr, pal.red("✗"), e)
			nErrs++
		}
		if len(o.errs) > 0 || !o.changed() {
			continue
		}
		changedDirs++
		changedFiles += len(o.writes) + len(o.deletes)
		if cfg.quiet {
			continue
		}

		fmt.Println(pal.bold(relDir(o.dir)))
		width := 0
		for _, m := range o.moves {
			if n := len(m.from) + len(m.dest); n > width {
				width = n
			}
		}
		for _, m := range o.moves {
			pad := strings.Repeat(" ", width-len(m.from)-len(m.dest))
			fmt.Printf("  %s %s %s%s  %s\n",
				pal.file(m.from), pal.dim("→"), pal.file(m.dest), pad, pal.dim(m.desc))
		}
		for _, base := range sortedKeys(o.creates) {
			fmt.Printf("  %s %s  %s\n", pal.green("+"), pal.file(base), pal.green(verb("created", "would create")))
		}
		for _, base := range sortedKeys(o.deletes) {
			fmt.Printf("  %s %s  %s\n", pal.red("-"), pal.file(base), pal.red(verb("deleted (empty)", "would delete (empty)")))
		}
		for _, base := range o.fmtOnly {
			fmt.Printf("  %s %s  %s\n", pal.dim("~"), pal.file(base), pal.dim(verb("reformatted", "needs reformatting")))
		}
		if cfg.diff {
			for _, base := range sortedKeys(o.writes) {
				printDiff(pal, filepath.Join(relDir(o.dir), base), o.origs[base], o.writes[base])
			}
			for _, base := range sortedKeys(o.deletes) {
				printDiff(pal, filepath.Join(relDir(o.dir), base), o.origs[base], nil)
			}
		}
		fmt.Println()
	}

	dur := elapsed.Round(time.Millisecond)
	if dur == 0 {
		dur = elapsed.Round(time.Microsecond)
	}
	summary := func(s string) {
		if !cfg.quiet {
			fmt.Println(s)
		}
	}

	switch {
	case nErrs > 0:
		fmt.Fprintln(os.Stderr, pal.red(fmt.Sprintf("✗ %s", plural(nErrs, "error", "errors"))))
		return 2
	case changedFiles > 0 && cfg.check:
		summary(pal.yellow(fmt.Sprintf("✗ %s in %s need changes", plural(changedFiles, "file", "files"), plural(changedDirs, "directory", "directories"))) +
			" " + pal.dim("(run tforg to apply)"))
		return 1
	case changedFiles > 0:
		summary(pal.green(fmt.Sprintf("✓ fixed %s in %s", plural(changedFiles, "file", "files"), plural(changedDirs, "directory", "directories"))) +
			" " + pal.dim(fmt.Sprintf("· %s", dur)))
		if cfg.staged {
			summary(pal.yellow("! commit aborted so you can review — stage the fixed files and commit again"))
		}
		return 1
	default:
		summary(pal.dim(fmt.Sprintf("✓ nothing to do · %s clean · %s", plural(totalFiles, "file", "files"), dur)))
		return 0
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
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
