package specroute

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
)

func TestPlanForTestSvc(t *testing.T) {
	dir := "../gofacts/testdata/svc"
	f, err := gofacts.Extract(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := PlanFor(dir, f)
	want := Plan{File: "internal/api/api.go", Func: "NewRouter", Mux: "mux", Import: "example.com/svc/internal/apispec"}
	if err != nil || got != want {
		t.Errorf("PlanFor = %+v, %v; want %+v", got, err, want)
	}
}

func TestPlanForRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", "package a\n\nfunc routes(mux, admin *http.ServeMux) {\n\tmux.HandleFunc(\"GET /a\", a)\n\tadmin.HandleFunc(\"GET /b\", b)\n}\n")
	route := func(pattern, at string) gofacts.Route {
		r := gofacts.Route{Pattern: pattern, Registered: at, Handler: "a.a"}
		r.Method, r.Path, _ = strings.Cut(pattern, " ")
		return r
	}
	for _, tt := range []struct {
		name   string
		routes []gofacts.Route
		want   string
	}{
		{"no routes", nil, "no route"},
		{"existing spec route", []gofacts.Route{route("GET /openapi.json", "a.go:4")}, "already serves"},
		{"two files", []gofacts.Route{route("GET /a", "a.go:4"), route("GET /c", "b.go:3")}, "a.go, b.go"},
		{"two muxes", []gofacts.Route{route("GET /a", "a.go:4"), route("GET /b", "a.go:5")}, "admin in routes, mux in routes"},
	} {
		if _, err := PlanFor(dir, &gofacts.Facts{Module: "m", Routes: tt.routes}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want one containing %q", tt.name, err, tt.want)
		}
	}

	ok := []gofacts.Route{route("GET /a", "a.go:4")}
	write("internal/apispec/apispec.go", "package apispec\n")
	if _, err := PlanFor(dir, &gofacts.Facts{Module: "m", Routes: ok}); err == nil || !strings.Contains(err.Error(), "apispec.go") {
		t.Errorf("hand-written apispec.go: err = %v", err)
	}
	write("internal/apispec/apispec.go", source)
	if p, err := PlanFor(dir, &gofacts.Facts{Module: "m", Routes: ok}); err != nil || p.Mux != "mux" {
		t.Errorf("generated apispec.go: %+v, %v", p, err)
	}
}

func TestInsert(t *testing.T) {
	plan := Plan{File: "router.go", Func: "NewRouter", Mux: "mux", Import: "catalog/internal/apispec"}
	for _, tt := range []struct {
		name     string
		plan     Plan
		src, out string
		err      string
	}{
		{
			name: "module group, sorted, trailing comment and nested registration",
			src: `package httpapi

import (
	"log/slog"
	"net/http"

	"catalog/internal/catalog"
)

// NewRouter returns the handler.
func NewRouter(store *catalog.Store, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index) // home
	if debug {
		mux.HandleFunc("GET /debug", h.debug)
	}

	return mux
}
`,
			out: `package httpapi

import (
	"log/slog"
	"net/http"

	"catalog/internal/apispec"
	"catalog/internal/catalog"
)

// NewRouter returns the handler.
func NewRouter(store *catalog.Store, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index) // home
	mux.Handle("GET /openapi.json", apispec.Handler())
	if debug {
		mux.HandleFunc("GET /debug", h.debug)
	}

	return mux
}
`,
		},
		{
			name: "std-only block gets a new group",
			src:  "package a\n\nimport (\n\t\"net/http\"\n)\n\nfunc NewRouter() {\n\tmux := http.NewServeMux()\n\tmux.Handle(\"GET /x\", x)\n}\n",
			out:  "package a\n\nimport (\n\t\"net/http\"\n\n\t\"catalog/internal/apispec\"\n)\n\nfunc NewRouter() {\n\tmux := http.NewServeMux()\n\tmux.Handle(\"GET /x\", x)\n\tmux.Handle(\"GET /openapi.json\", apispec.Handler())\n}\n",
		},
		{
			name: "single import, method, default mux, CRLF",
			plan: Plan{File: "a.go", Func: "(*server).routes", Mux: "http", Import: "catalog/internal/apispec"},
			src:  "package a\r\n\r\nimport \"net/http\"\r\n\r\nfunc (s *server) routes() {\r\n\thttp.HandleFunc(\"/x\", x)\r\n}\r\n",
			out:  "package a\r\n\r\nimport \"net/http\"\r\nimport \"catalog/internal/apispec\"\r\n\r\nfunc (s *server) routes() {\r\n\thttp.HandleFunc(\"/x\", x)\r\n\thttp.Handle(\"GET /openapi.json\", apispec.Handler())\r\n}\r\n",
		},
		{
			name: "already there, under another import name",
			src:  "package a\n\nimport (\n\tspec \"catalog/internal/apispec\"\n)\n\nfunc NewRouter() {\n\tmux.Handle(\"GET /x\", x)\n\tmux.Handle(\"GET /openapi.json\", spec.Handler())\n}\n",
			out:  "package a\n\nimport (\n\tspec \"catalog/internal/apispec\"\n)\n\nfunc NewRouter() {\n\tmux.Handle(\"GET /x\", x)\n\tmux.Handle(\"GET /openapi.json\", spec.Handler())\n}\n",
		},
		{
			name: "two registrations on a line",
			src:  "package a\n\nfunc NewRouter() {\n\tmux.Handle(\"GET /x\", x); f()\n}\n",
			err:  "line of its own",
		},
		{
			name: "only nested registrations",
			src:  "package a\n\nfunc NewRouter() {\n\tfor _, r := range rs {\n\t\tmux.Handle(r.p, r.h)\n\t}\n}\n",
			err:  "as a statement of its own",
		},
		{
			name: "apispec name taken",
			src:  "package a\n\nimport apispec \"other/spec\"\n\nfunc NewRouter() {\n\tmux.Handle(\"GET /x\", x)\n}\n",
			err:  "already imports other/spec",
		},
		{
			name: "missing func",
			src:  "package a\n\nfunc other() {}\n",
			err:  "no func NewRouter",
		},
	} {
		p := plan
		if tt.plan != (Plan{}) {
			p = tt.plan
		}
		got, err := Insert([]byte(tt.src), p)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: err = %v, want one containing %q", tt.name, err, tt.err)
			}
			continue
		}
		if err != nil || string(got) != tt.out {
			t.Errorf("%s: err = %v, got:\n%s\nwant:\n%s", tt.name, err, got, tt.out)
			continue
		}
		if again, err := Insert(got, p); err != nil || string(again) != string(got) {
			t.Errorf("%s: a second Insert changed the file (err %v):\n%s", tt.name, err, again)
		}
	}
}
