package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
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
