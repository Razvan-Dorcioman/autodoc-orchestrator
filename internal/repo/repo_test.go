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
