package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo creates a temp git repository and returns its path.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func inDir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestStagedTfFiles(t *testing.T) {
	repo := gitRepo(t)
	sub := filepath.Join(repo, "modules", "a")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "staged.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(sub, "nested.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "unstaged.tf"), []byte("locals {}\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("x"), 0o644)

	cmd := exec.Command("git", "add", "staged.tf", "modules/a/nested.tf", "staged.txt")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	inDir(t, sub) // works from a subdirectory too
	files, errs := StagedTfFiles()
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 staged .tf files, got %v", files)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if base != "staged.tf" && base != "nested.tf" {
			t.Errorf("unexpected file %s", f)
		}
	}
}

func TestInstallHook(t *testing.T) {
	repo := gitRepo(t)
	inDir(t, repo)

	if code := InstallHook(nil); code != 0 {
		t.Fatalf("install failed with code %d", code)
	}
	path := filepath.Join(repo, ".git", "hooks", "pre-commit")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Error("hook not executable")
	}
	b, _ := os.ReadFile(path)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// The hook must not depend on PATH: GUI git clients don't have
	// shell-profile PATH additions, so the binary path is embedded.
	if !strings.Contains(string(b), `TFORG="`+exe+`"`) {
		t.Errorf("hook must embed the absolute binary path:\n%s", b)
	}
	if !strings.Contains(string(b), `exec "$TFORG" -staged`) {
		t.Errorf("hook script wrong:\n%s", b)
	}

	if code := InstallHook(nil); code != 2 {
		t.Error("second install without -force must refuse")
	}
	if code := InstallHook([]string{"-force"}); code != 0 {
		t.Error("-force should overwrite")
	}
}
