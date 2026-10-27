package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareClonesLocalRepoOntoBranch(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	mustGit(t, src, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", ".")
	mustGit(t, src, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")
	sha := mustGit(t, src, "rev-parse", "HEAD")

	dst := filepath.Join(t.TempDir(), "work")
	r, err := Prepare(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if r.BaseSHA != sha || r.Branch != "autodoc/"+sha[:12] {
		t.Errorf("repo = %+v, want base %s on autodoc/%s", r, sha, sha[:12])
	}
	if got := mustGit(t, dst, "branch", "--show-current"); got != r.Branch {
		t.Errorf("checked out %q, want %q", got, r.Branch)
	}
	if got := mustGit(t, src, "branch", "--show-current"); got != "main" {
		t.Errorf("source repo moved to %q; it must stay untouched", got)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || string(data) != "a\n" {
		t.Errorf("a.txt = %q, %v; want the committed bytes, without CRLF conversion", data, err)
	}
}

func TestShowAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	mustGit(t, dir, "config", "core.autocrlf", "false") // as Prepare's clone has it
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")

	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\n// @title X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(dir, "internal", "apispec", "swagger.json")
	if err := os.MkdirAll(filepath.Dir(gen), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gen, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := Show(ctx, dir, "HEAD", "a.go"); err != nil || string(got) != "package a\n" {
		t.Errorf("Show = %q, %v; want the committed bytes", got, err)
	}
	if err := Restore(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(got) != "package a\n" {
		t.Errorf("after Restore a.go = %q", got)
	}
	if _, err := os.Stat(gen); !os.IsNotExist(err) {
		t.Errorf("after Restore the generated file still exists: %v", err)
	}
}

func TestPrepareFailsOnMissingRepo(t *testing.T) {
	_, err := Prepare(context.Background(), filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "work"))
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Errorf("err = %v, want a git clone error", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
