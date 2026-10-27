package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const handlersSrc = `package httpapi

import "net/http"

type handlers struct{}

// product writes one product.
func (h *handlers) product(w http.ResponseWriter, r *http.Request) {}

func (h *handlers) healthz(w http.ResponseWriter, r *http.Request) {}

var x = 1; func inline() {}
`

func writeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"internal/httpapi/handlers.go":      handlersSrc,
		"internal/httpapi/handlers_test.go": "package httpapi\n\nfunc helper() {}\n",
		"README.md":                         "# x\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPatchCommentAccepts(t *testing.T) {
	tests := []struct {
		name  string
		calls []PatchCommentArgs
		want  string // the declaration as it reads afterwards
	}{
		{
			name:  "after the existing doc comment",
			calls: []PatchCommentArgs{{File: "internal/httpapi/handlers.go", Func: "(*handlers).product", Lines: []string{"// @Summary Get a product", "// @Router /products/{id} [get]"}}},
			want:  "// product writes one product.\n// @Summary Get a product\n// @Router /products/{id} [get]\nfunc (h *handlers) product(",
		},
		{
			name:  "no doc comment, package-qualified name as in facts.json",
			calls: []PatchCommentArgs{{File: "internal/httpapi/handlers.go", Func: "httpapi.(*handlers).healthz", Lines: []string{"// @Router /healthz [get]"}}},
			want:  "\n\n// @Router /healthz [get]\nfunc (h *handlers) healthz(",
		},
		{
			name: "a second call replaces the first set",
			calls: []PatchCommentArgs{
				{File: "internal/httpapi/handlers.go", Func: "(*handlers).product", Lines: []string{"// @Summary Old", "// @Router /old [get]"}},
				{File: "internal/httpapi/handlers.go", Func: "(*handlers).product", Lines: []string{"// @Summary New"}},
			},
			want: "// product writes one product.\n// @Summary New\nfunc (h *handlers) product(",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeRepo(t)
			var res PatchCommentResult
			for _, c := range tt.calls {
				var err error
				if res, err = PatchComment(root, c); err != nil {
					t.Fatalf("PatchComment(%+v): %v", c, err)
				}
			}
			got, err := os.ReadFile(filepath.Join(root, "internal/httpapi/handlers.go"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), tt.want) {
				t.Errorf("file does not contain %q:\n%s", tt.want, got)
			}
			if !strings.Contains(tt.want, res.Comment) {
				t.Errorf("result comment %q is not the doc comment in %q", res.Comment, tt.want)
			}
		})
	}
}

func TestPatchCommentRejects(t *testing.T) {
	const file = "internal/httpapi/handlers.go"
	ok := []string{"// @Summary S"}
	tests := []struct {
		name string
		args PatchCommentArgs
		msg  string // part of the error
	}{
		{"code line", PatchCommentArgs{File: file, Func: "main", Lines: []string{"var y = 2"}}, "not a swag annotation"},
		{"plain comment", PatchCommentArgs{File: file, Func: "(*handlers).product", Lines: []string{"// product does more."}}, "not a swag annotation"},
		{"struct tag smuggled in a line break", PatchCommentArgs{File: file, Func: "(*handlers).product", Lines: []string{"// @Summary S\ntype T struct{}"}}, "line break"},
		{"block comment", PatchCommentArgs{File: file, Func: "(*handlers).product", Lines: []string{"/* @Summary S */"}}, "not a swag annotation"},
		{"bare at sign", PatchCommentArgs{File: file, Func: "(*handlers).product", Lines: []string{"// @ "}}, "not a swag annotation"},
		{"no lines", PatchCommentArgs{File: file, Func: "(*handlers).product"}, "empty"},
		{"unknown func", PatchCommentArgs{File: file, Func: "(*handlers).missing", Lines: ok}, "it declares: (*handlers).product, (*handlers).healthz, inline"},
		{"func not at line start", PatchCommentArgs{File: file, Func: "inline", Lines: ok}, "does not start its line"},
		{"not a go file", PatchCommentArgs{File: "README.md", Func: "main", Lines: ok}, "not a non-test .go file"},
		{"test file", PatchCommentArgs{File: "internal/httpapi/handlers_test.go", Func: "helper", Lines: ok}, "not a non-test .go file"},
		{"escapes the root", PatchCommentArgs{File: "../x.go", Func: "main", Lines: ok}, "outside the repository"},
		{"missing file", PatchCommentArgs{File: "nope.go", Func: "main", Lines: ok}, "read nope.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeRepo(t)
			_, err := PatchComment(root, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("PatchComment() error = %v, want it to contain %q", err, tt.msg)
			}
			if strings.Contains(err.Error(), root) {
				t.Errorf("error %q leaks the workdir path", err)
			}
			got, _ := os.ReadFile(filepath.Join(root, file))
			if string(got) != handlersSrc {
				t.Errorf("rejected patch changed the file:\n%s", got)
			}
		})
	}
}

func TestSameCode(t *testing.T) {
	before := []byte("package a\n\nfunc f() {}\n")
	if err := sameCode("a.go", before, []byte("package a\n\n// @Summary F\nfunc f() {}\n")); err != nil {
		t.Errorf("comment-only change rejected: %v", err)
	}
	if err := sameCode("a.go", before, []byte("package a\n\nfunc f() { _ = 1 }\n")); err == nil {
		t.Error("code change accepted")
	}
}

func TestFuncSource(t *testing.T) {
	root := writeRepo(t)
	got, err := FuncSource(root, "internal/httpapi/handlers.go", "(*handlers).product")
	if err != nil {
		t.Fatal(err)
	}
	want := "// product writes one product.\nfunc (h *handlers) product(w http.ResponseWriter, r *http.Request) {}"
	if got != want {
		t.Errorf("FuncSource() = %q, want %q", got, want)
	}
}

func TestPatchScope(t *testing.T) {
	root := writeRepo(t)
	path := filepath.Join(root, "internal", "httpapi", "handlers.go")
	scope := &PatchScope{}
	p, err := newPatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	p.scope = scope
	product := PatchCommentArgs{File: "internal/httpapi/handlers.go", Func: "(*handlers).product", Lines: []string{"// @Summary Product"}}
	healthz := PatchCommentArgs{File: "internal/httpapi/handlers.go", Func: "(*handlers).healthz", Lines: []string{"// @Summary Health"}}

	// Unset: any function.
	if _, err := p.patch(healthz); err != nil {
		t.Fatalf("unscoped patch: %v", err)
	}

	scope.Set("internal/httpapi/handlers.go", "(*handlers).product")
	before, _ := os.ReadFile(path)
	_, err = p.patch(healthz)
	if err == nil || !strings.Contains(err.Error(), `func "(*handlers).product"`) {
		t.Fatalf("patch of another function: err = %v, want a rejection naming the scoped func", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a rejected patch changed the file")
	}

	// The scoped function passes, also under its package-qualified name.
	product.Func = "httpapi.(*handlers).product"
	if _, err := p.patch(product); err != nil {
		t.Fatalf("scoped patch: %v", err)
	}

	scope.Clear()
	if _, err := p.patch(healthz); err != nil {
		t.Fatalf("patch after Clear: %v", err)
	}
}
