package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/FutureFrenzy96/tforg/internal/engine"
)

func mkTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, rel := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("locals {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// relKeys returns the collected directories relative to root, each mapped to
// its sorted base names, so assertions don't depend on the temp path.
func relKeys(t *testing.T, root string, targets map[string][]string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for dir, bases := range targets {
		r, err := filepath.Rel(root, dir)
		if err != nil {
			t.Fatal(err)
		}
		out[r] = bases
	}
	return out
}

func TestCollectTargetsHandlesDeepNesting(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"main.tf",
		"modules/a/main.tf",
		"modules/a/b/main.tf",
		"modules/a/b/c/main.tf",
		"modules/a/b/c/d/main.tf",  // 4 levels deep
		"modules/a/b/c/d/extra.tf", // two files in the deepest dir
	)

	targets, errs := collectTargets([]string{root})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	got := relKeys(t, root, targets)
	want := map[string][]string{
		".":               {"main.tf"},
		"modules/a":       {"main.tf"},
		"modules/a/b":     {"main.tf"},
		"modules/a/b/c":   {"main.tf"},
		"modules/a/b/c/d": {"extra.tf", "main.tf"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nesting not grouped per directory:\n got  %v\n want %v", got, want)
	}
}

func TestCollectTargetsSkipsVendorAndHiddenDirs(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"main.tf",
		".terraform/modules/x/main.tf",
		".git/hooks/main.tf",
		"node_modules/pkg/main.tf",
		".hidden/main.tf",
		"real/main.tf",
	)

	targets, errs := collectTargets([]string{root})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	got := relKeys(t, root, targets)
	dirs := make([]string, 0, len(got))
	for d := range got {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	want := []string{".", "real"}
	if !reflect.DeepEqual(dirs, want) {
		t.Errorf("vendor/hidden dirs not skipped: got %v, want %v", dirs, want)
	}
}

func TestCollectTargetsSingleNestedFile(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root, "modules/a/b/c/main.tf", "modules/a/b/c/data.tf")

	// Targeting a single deep file should collect only that file's directory.
	targets, errs := collectTargets([]string{filepath.Join(root, "modules/a/b/c/main.tf")})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	got := relKeys(t, root, targets)
	want := map[string][]string{"modules/a/b/c": {"main.tf"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single deep file: got %v, want %v", got, want)
	}
}

// captureRun executes Run with stdout captured, returning output and exit code.
func captureRun(t *testing.T, args []string) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Run(args, "test")
	w.Close()
	os.Stdout = old
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String(), code
}

func TestRunWarnsWhenNoTfFilesFound(t *testing.T) {
	empty := t.TempDir()
	out, code := captureRun(t, []string{empty})
	if code != 0 {
		t.Fatalf("empty scope should exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "no .tf files found under "+empty) {
		t.Errorf("expected scope warning, got:\n%s", out)
	}
}

func TestRunWarnsWhenEverythingExcluded(t *testing.T) {
	dir := t.TempDir()
	mkTree(t, dir, "gen/a.tf", "gen/b.tf")
	out, code := captureRun(t, []string{"-exclude", "gen", dir})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "2 files excluded by ignore/exclude patterns") {
		t.Errorf("expected exclusion warning, got:\n%s", out)
	}
}

func TestCheckSummaryShowsCheckedFileCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dirty.tf"), []byte("variable \"a\" {\ntype=string\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clean.tf"), []byte("locals {\n  a = 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := captureRun(t, []string{"-check", "-no-config", dir})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "checked 2 files in 1 directory") {
		t.Errorf("check summary should state the scanned scope, got:\n%s", out)
	}

	// Clean tree: the count is in the nothing-to-do line.
	cleanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cleanDir, "locals.tf"), []byte("locals {\n  a = 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = captureRun(t, []string{"-check", "-no-config", cleanDir})
	if code != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", code, out)
	}
	if !strings.Contains(out, "1 file clean") {
		t.Errorf("clean summary should state the file count, got:\n%s", out)
	}
}

func TestMapFlagPatternSyntax(t *testing.T) {
	dest := map[string]string{}
	var rules []engine.PlaceRule
	m := &mapFlag{dest: dest, rules: &rules}

	if err := m.Set("terraform=terraform.tf,module:network_data=data.tf"); err != nil {
		t.Fatal(err)
	}
	if dest["terraform"] != "terraform.tf" {
		t.Errorf("plain override lost: %v", dest)
	}
	if len(rules) != 1 || rules[0].Pattern != "network_data" || rules[0].File != "data.tf" {
		t.Errorf("pattern rule wrong: %+v", rules)
	}
	for _, bad := range []string{"module:=x.tf", ":m=x.tf", "module:m=dir/x.tf", "nope"} {
		if err := m.Set(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
