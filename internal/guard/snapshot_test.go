package guard

import (
	"os"
	"path/filepath"
	"testing"
)

const base = `package p

type T struct {
	A int ` + "`json:\"a\"`" + `
}

func F(x int) int {
	return x + 1
}
`

func TestCodeHashIgnoresCommentsAndLayout(t *testing.T) {
	same := []string{
		// swag annotations above a func
		"package p\n\ntype T struct {\n\tA int `json:\"a\"`\n}\n\n// F adds one.\n// @Summary Add one\n// @Success 200 {object} T\nfunc F(x int) int {\n\treturn x + 1\n}\n",
		// block comment and different layout
		"package p\n/* doc */\ntype T struct { A int `json:\"a\"` }\nfunc F(x int) int { return x + 1 }\n",
		// explicit semicolons
		"package p; type T struct { A int `json:\"a\"` }; func F(x int) int { return x + 1 };",
	}
	want := mustHash(t, base)
	for i, src := range same {
		if got := mustHash(t, src); got != want {
			t.Errorf("variant %d: hash differs, want same", i)
		}
	}
}

func TestCodeHashDetectsCodeChanges(t *testing.T) {
	changed := map[string]string{
		"literal":    "package p\n\ntype T struct {\n\tA int `json:\"a\"`\n}\n\nfunc F(x int) int {\n\treturn x + 2\n}\n",
		"struct tag": "package p\n\ntype T struct {\n\tA int `json:\"a\" example:\"1\"`\n}\n\nfunc F(x int) int {\n\treturn x + 1\n}\n",
		"identifier": "package p\n\ntype T struct {\n\tA int `json:\"a\"`\n}\n\nfunc F(y int) int {\n\treturn y + 1\n}\n",
	}
	want := mustHash(t, base)
	for name, src := range changed {
		if got := mustHash(t, src); got == want {
			t.Errorf("%s change: hash unchanged, want different", name)
		}
	}
}

func TestCodeHashRejectsInvalidGo(t *testing.T) {
	if _, err := CodeHash("bad.go", []byte("package p\nvar s = \"unterminated\n")); err == nil {
		t.Error("want an error for invalid Go")
	}
}

func TestTake(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), base)
	write(t, filepath.Join(dir, "docs", "x.md"), "# x\n")
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	s, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 2 {
		t.Fatalf("files = %v, want a.go and docs/x.md only", s.Files)
	}
	if s.Files["a.go"].CodeHash == "" || s.Files["docs/x.md"].CodeHash != "" || s.Files["docs/x.md"].SHA256 == "" {
		t.Errorf("snapshot = %+v", s.Files)
	}
}

func mustHash(t *testing.T, src string) string {
	t.Helper()
	h, err := CodeHash("x.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
