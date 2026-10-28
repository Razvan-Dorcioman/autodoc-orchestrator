package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/repo"
	"autodoc/internal/trace"
)

func TestCommit(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := repo.Git(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(dir, rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(src, "init", "--quiet", "-b", "main")
	write(src, "main.go", "package main\n\nfunc main() {}\n")
	write(src, "main_test.go", "package main\n")
	git(src, "add", ".")
	git(src, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "init")

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	r, err := repo.Prepare(ctx, src, filepath.Join(rec.Dir(), "work"))
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{Repo: r, Main: "main.go"}

	if detail, err := commit(ctx, rec, res); err != nil || res.Commit != "" || !strings.Contains(detail, "nothing to commit") {
		t.Errorf("unchanged: %q, %v, commit %q", detail, err, res.Commit)
	}

	write(r.Dir, "main_test.go", "package main\n\n// @title X\n")
	if _, err := commit(ctx, rec, res); err == nil || !strings.Contains(err.Error(), "main_test.go") {
		t.Errorf("a test file changed: err = %v, want one naming it", err)
	}
	if err := repo.Restore(ctx, r.Dir); err != nil {
		t.Fatal(err)
	}
	if out := git(r.Dir, "status", "--porcelain"); out != "" {
		t.Errorf("after Restore, staged changes are left:\n%s", out)
	}

	write(r.Dir, "main.go", "package main\n\n// @title X\nfunc main() {}\n")
	write(r.Dir, "internal/apispec/swagger.json", "{}\n")
	write(r.Dir, "docs/api.md", "# API\n")
	detail, err := commit(ctx, rec, res)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != git(r.Dir, "rev-parse", "HEAD") || strings.Join(res.Files, " ") != "docs/api.md internal/apispec/swagger.json main.go" {
		t.Errorf("commit = %q, files %v (%s)", res.Commit, res.Files, detail)
	}
	if branch := git(r.Dir, "branch", "--show-current"); branch != r.Branch {
		t.Errorf("committed on %q, want %q", branch, r.Branch)
	}
	msg := git(r.Dir, "log", "-1", "--format=%B")
	for _, want := range []string{"Add API annotations, OpenAPI spec and docs", r.BaseSHA, "Run: " + rec.RunID()} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if git(src, "branch", "--list", r.Branch) != "" {
		t.Error("the run branch reached the source repository without a push")
	}
	if o := res.outcome(); o.Commit != res.Commit || o.Branch != r.Branch || o.PushedTo != "" {
		t.Errorf("outcome = %+v", o)
	}
}

// TestReindex: annotations above a handler move it down; the re-extracted
// facts carry the new position, and a change to the routes is refused.
func TestReindex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../gofacts/testdata/svc")); err != nil {
		t.Fatal(err)
	}
	before, err := gofacts.Extract(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "internal", "api", "api.go")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	annotated := strings.Replace(string(data), "\nfunc index(", "\n// @Summary List items\n// @Router / [get]\nfunc index(", 1)
	if err := os.WriteFile(file, []byte(annotated), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := reindex(ctx, dir, before, filepath.Join(t.TempDir(), "facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if b, a := before.Routes[0].HandlerPos, after.Routes[0].HandlerPos; b != "internal/api/api.go:51" || a != "internal/api/api.go:53" {
		t.Errorf("index handler at %s before, %s after; want 51 and 53", b, a)
	}

	before.Routes = before.Routes[1:]
	if _, err := reindex(ctx, dir, before, filepath.Join(t.TempDir(), "facts.json")); err == nil || !strings.Contains(err.Error(), "routes changed") {
		t.Errorf("changed routes: err = %v", err)
	}
}
