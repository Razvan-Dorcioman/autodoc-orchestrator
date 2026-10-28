package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteCodeLinks(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"internal/api/h.go", "Makefile", "docs/api.md"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct{ name, in, want string }{
		{"code link with a line", "See [index](internal/api/h.go:31).", "See index (`internal/api/h.go:31`)."},
		{"text is the target", "[`internal/api/h.go:31`](internal/api/h.go:31)", "`internal/api/h.go:31`"},
		{"from the root", "[h](/internal/api/h.go:Index)", "h (`internal/api/h.go:Index`)"},
		{"relative with a line", "[h](../internal/api/h.go:31)", "h (`internal/api/h.go:31`)"},
		{"github anchor", "[h](internal/api/h.go#L3-L9)", "h (`internal/api/h.go:3-9`)"},
		{"two on a line", "[a](Makefile) and [b](internal/api/h.go)", "a (`Makefile`) and b (`internal/api/h.go`)"},
		{"page link", "[API](api.md)", "[API](api.md)"},
		{"resolving link", "[h](../internal/api/h.go)", "[h](../internal/api/h.go)"},
		{"missing file", "[x](internal/api/gone.go:3)", "[x](internal/api/gone.go:3)"},
		{"docs page from the root", "[a](docs/api.md:3)", "[a](docs/api.md:3)"},
		{"web link", "[go](https://go.dev/x.go)", "[go](https://go.dev/x.go)"},
		{"image", "![h](internal/api/h.go)", "![h](internal/api/h.go)"},
		{"inside inline code", "`[h](internal/api/h.go:3)`", "`[h](internal/api/h.go:3)`"},
		{"inside a fence", "```\n[h](internal/api/h.go:3)\n```", "```\n[h](internal/api/h.go:3)\n```"},
		{"outside the repo", "[h](../../h.go)", "[h](../../h.go)"},
	} {
		got, n := rewriteCodeLinks(dir, "docs/request-flow.md", tt.in)
		if got != tt.want {
			t.Errorf("%s: %q -> %q, want %q", tt.name, tt.in, got, tt.want)
		}
		if changed := got != tt.in; changed != (n > 0) {
			t.Errorf("%s: count %d for a change of %v", tt.name, n, changed)
		}
	}
}

// TestRewriteCodeLinksThenGateB: a page that links code fails gate B's link
// check as written and passes it once the links are rewritten.
func TestRewriteCodeLinksThenGateB(t *testing.T) {
	pages := goodDocs()
	pages["docs/request-flow.md"] = "# Request flow\n\nThe mux calls `api.handlers.item` ([item](internal/api/api.go:12)).\n"
	in := docsDir(t, pages)
	if rep := GateB(in); rep.OK() {
		t.Fatal("gate B accepts a link to code")
	}
	n, err := RewriteCodeLinks(in.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := RewriteSummary(n); got != "rewrote 1 link(s) to code as citations (request-flow.md: 1)" {
		t.Errorf("summary = %q", got)
	}
	if rep := GateB(in); !rep.OK() {
		t.Errorf("after the rewrite:\n%s", rep)
	}
}
