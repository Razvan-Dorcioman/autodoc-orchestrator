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

func TestCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n")
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")
	inDocs := func(p string) bool { return p == "a.go" || strings.HasPrefix(p, "docs/") }

	if sha, files, err := Commit(ctx, dir, "nothing", inDocs); sha != "" || files != nil || err != nil {
		t.Errorf("clean tree: Commit = %q, %v, %v; want no commit", sha, files, err)
	}

	write("a.go", "package a\n\n// @title X\n")
	write("docs/api.md", "# API\n")
	write("secrets.txt", "x")
	if _, _, err := Commit(ctx, dir, "out of scope", inDocs); err == nil || !strings.Contains(err.Error(), "secrets.txt") {
		t.Errorf("out of scope: err = %v, want one naming secrets.txt", err)
	}
	if err := os.Remove(filepath.Join(dir, "secrets.txt")); err != nil {
		t.Fatal(err)
	}

	sha, files, err := Commit(ctx, dir, "Add docs\n\nRun: r1", inDocs)
	if err != nil {
		t.Fatal(err)
	}
	if sha != mustGit(t, dir, "rev-parse", "HEAD") || strings.Join(files, " ") != "a.go docs/api.md" {
		t.Errorf("Commit = %q, %v", sha, files)
	}
	if got := mustGit(t, dir, "log", "-1", "--format=%an <%ae>|%B"); got != Author+"|Add docs\n\nRun: r1" {
		t.Errorf("commit = %q", got)
	}
}

func TestPushNeedsHTTPSAndHidesTheToken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	mustGit(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "src"))
	if _, err := Push(ctx, dir, "main", "s3cret"); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("local origin: err = %v, want a refusal", err)
	}

	mustGit(t, dir, "remote", "set-url", "origin", "https://127.0.0.1:1/nobody/repo.git")
	url, err := Push(ctx, dir, "main", "s3cret")
	if err == nil || url != "https://127.0.0.1:1/nobody/repo.git" {
		t.Fatalf("unreachable origin: %q, %v; want an error", url, err)
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "Authorization") {
		t.Errorf("the error leaks the credentials: %v", err)
	}
	if cfg := mustGit(t, dir, "config", "--list", "--local"); strings.Contains(cfg, "extraheader") {
		t.Errorf("the token header was written to .git/config:\n%s", cfg)
	}
}

func TestPushLocalAddsOnlyTheBranch(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	mustGit(t, src, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, src, "add", ".")
	mustGit(t, src, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")
	// Uncommitted work in the source must survive the push.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	push := func() string {
		t.Helper()
		r, err := Prepare(ctx, src, filepath.Join(t.TempDir(), "work"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.Dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sha, _, err := Commit(ctx, r.Dir, "add b", func(string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := PushLocal(ctx, r.Dir, r.Branch); err != nil {
			t.Fatal(err)
		}
		if got := mustGit(t, src, "rev-parse", r.Branch); got != sha {
			t.Errorf("%s in the source = %s, want the run's commit %s", r.Branch, got, sha)
		}
		return sha
	}
	first := push()
	if second := push(); second == first {
		t.Errorf("the second run's commit %s equals the first's", second)
	}
	if got := mustGit(t, src, "branch", "--show-current"); got != "main" {
		t.Errorf("source checked out %q, want main", got)
	}
	if data, _ := os.ReadFile(filepath.Join(src, "a.txt")); string(data) != "wip\n" {
		t.Errorf("a.txt in the source = %q, want the uncommitted edit kept", data)
	}
	if _, err := os.Stat(filepath.Join(src, "b.txt")); err == nil {
		t.Error("b.txt appeared in the source's working tree")
	}
}
