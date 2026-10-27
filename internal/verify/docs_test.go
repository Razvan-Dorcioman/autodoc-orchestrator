package verify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
)

const docsAPIGo = `package api

const (
	defaultQty = 1
	maxQty     = 10
)

type Kind string

const KindBook Kind = "book"

type Item struct {
	ID   int    ` + "`json:\"id\"`" + `
	Kind string ` + "`json:\"kind\"`" + `
}

type ErrorBody struct {
	Code string ` + "`json:\"code\"`" + `
}

type handlers struct{}

func (h *handlers) item() {}

func writeError(code string) {}
`

// docsRepo is a small service with the files the facts point at.
var docsRepo = map[string]string{
	"go.mod":              "module svc\n",
	"Makefile":            "PORT ?= 8080\n\nrun:\n\tPORT=$(PORT) go run ./cmd/server\n\ntest:\n\tgo test ./...\n",
	"cmd/server/main.go":  "package main\n\nfunc main() {}\n",
	"internal/api/api.go": docsAPIGo,
}

func docsFacts() *gofacts.Facts {
	return &gofacts.Facts{
		Packages: []gofacts.Package{
			{Path: "svc/cmd/server", Name: "main", Dir: "cmd/server", Files: []string{"cmd/server/main.go"}},
			{Path: "svc/internal/api", Name: "api", Dir: "internal/api", Files: []string{"internal/api/api.go"}},
		},
		Routes: []gofacts.Route{{
			Pattern: "GET /items/{id}", Method: "GET", Path: "/items/{id}", Handler: "api.(*handlers).item",
			Params: []gofacts.Param{{Name: "id", In: "path"}, {Name: "qty", In: "query"}},
			Responses: []gofacts.ResponseSite{
				{Status: 200, Body: "api.Item"},
				{Status: 404, Code: "item_not_found", Body: "api.ErrorBody"},
			},
		}},
		Responses: []gofacts.ResponseSite{
			{Status: 200, Body: "api.Item"},
			{Status: 404, Code: "item_not_found", Body: "api.ErrorBody", Func: "api.(*handlers).item", Pos: "internal/api/api.go:30"},
		},
		ImplicitResponses: []gofacts.ImplicitResponse{{Status: 405, Source: "net/http.ServeMux"}, {Status: 200, Source: "net/http.ServeMux"}},
		Types: []gofacts.Type{
			{Name: "Item", Package: "api", Underlying: "struct", Fields: []gofacts.Field{{Name: "ID", Type: "int", JSON: "id"}, {Name: "Kind", Type: "string", JSON: "kind"}}},
			{Name: "ErrorBody", Package: "api", Underlying: "struct", Fields: []gofacts.Field{{Name: "Code", Type: "string", JSON: "code"}}},
		},
		Constants: []gofacts.Constant{
			{Name: "defaultQty", Package: "api", Type: "untyped int", Value: "1"},
			{Name: "maxQty", Package: "api", Type: "untyped int", Value: "10"},
			{Name: "KindBook", Package: "api", Type: "api.Kind", Value: "book"},
		},
		Env:            []gofacts.EnvVar{{Name: "PORT", Default: "8080"}},
		ServerSettings: []gofacts.Setting{{Name: "http.Server.Addr"}, {Name: "http.Server.ReadTimeout", Value: "5 * time.Second"}},
		MakeTargets:    []gofacts.MakeTarget{{Name: "run"}, {Name: "test"}},
	}
}

// goodDocs mention every fact and reference only what exists.
func goodDocs() map[string]string {
	return map[string]string{
		"docs/overview.md":       "# Overview\n\nAn item service; see [the API](api.md) and [the code](../internal/api/api.go).\n",
		"docs/structure.md":      "# Structure\n\n- `cmd/server`: the command, `cmd/server/main.go:main`.\n- `internal/api`: handlers, `internal/api/api.go:(*handlers).item`, in `api.go`.\n",
		"docs/request-flow.md":   "# Request flow\n\nThe mux calls `api.handlers.item` (`internal/api/api.go:12`).\n",
		"docs/api.md":            "# API\n\n## `GET /items/{id}`\n\nPath `id`, query `qty`. Returns `id` and `kind`.\n\n```sh\ncurl localhost:8080/items/1?qty=2\n```\n\nAlso `GET /items/7` and `GET /openapi.json`.\n",
		"docs/business-rules.md": "# Business rules\n\n`qty` defaults to `defaultQty` and is capped at `maxQty` (`internal/api/api.go:maxQty`). Kinds: `book`.\n",
		"docs/errors.md":         "# Errors\n\n`item_not_found` (404), body field `code`, from `api.writeError`. A wrong method gives 405.\n",
		"docs/configuration.md":  "# Configuration\n\n| Variable | Default |\n|---|---|\n| `PORT` | 8080 |\n\nThe server sets `ReadTimeout`. `CGO_ENABLED` and `GOOS` are toolchain settings.\n",
		"docs/testing.md":        "# Test and ship\n\n```sh\nmake test\nPORT=9090 make run\n```\n\nTargets: `Makefile:run` and `Makefile:test`.\n",
	}
}

// docsDir writes the repo, fingerprints it as the docs step's starting
// point, then writes pages.
func docsDir(t *testing.T, pages map[string]string) GateBInput {
	t.Helper()
	dir, before := snapshotOf(t, docsRepo)
	for rel, content := range pages {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return GateBInput{Dir: dir, Facts: docsFacts(), Before: before}
}

func TestGateBGreen(t *testing.T) {
	rep := GateB(docsDir(t, goodDocs()))
	if !rep.OK() {
		t.Fatalf("GateB on good docs:\n%s", rep)
	}
}

func TestDocMentions(t *testing.T) {
	got := map[string][]string{}
	for page, ms := range DocMentions(docsFacts()) {
		for _, m := range ms {
			got[page] = append(got[page], m.Kind+" "+m.Text)
		}
	}
	want := map[string][]string{
		"docs/structure.md":      {"package cmd/server", "package internal/api"},
		"docs/api.md":            {"route GET /items/{id}", "param id", "param qty", "field id", "field kind"},
		"docs/errors.md":         {"field code", "error_code item_not_found", "status 405"},
		"docs/business-rules.md": {"constant defaultQty", "constant maxQty", "constant KindBook"},
		"docs/configuration.md":  {"env PORT", "setting ReadTimeout"},
		"docs/testing.md":        {"make_target run", "make_target test"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DocMentions =\n%v\nwant\n%v", got, want)
	}
}

func TestGateBFindings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		page  string
		edit  func(string) string
		check string
		where string
		want  string
	}{
		{"missing page", "docs/testing.md", func(string) string { return "" }, "docs", "docs/testing.md", "not written"},
		{"no title", "docs/overview.md", func(s string) string { return strings.TrimPrefix(s, "# ") }, "docs", "docs/overview.md:1", "title"},
		{"missing error code", "docs/errors.md", func(s string) string { return strings.ReplaceAll(s, "`item_not_found`", "the error") }, "coverage", "docs/errors.md", "item_not_found"},
		{"route on its own is not enough", "docs/api.md", func(s string) string { return strings.ReplaceAll(s, "GET /items/{id}", "/items/{id}") }, "coverage", "docs/api.md", "GET /items/{id}"},
		{"missing constant", "docs/business-rules.md", func(s string) string { return strings.ReplaceAll(s, "`book`", "books") }, "coverage", "docs/business-rules.md", "KindBook` (or book)"},
		{"unknown symbol in file", "docs/structure.md", func(s string) string { return strings.ReplaceAll(s, "(*handlers).item", "(*handlers).list") }, "reference", "docs/structure.md:4", "declares no"},
		{"unknown file", "docs/structure.md", func(s string) string { return strings.ReplaceAll(s, "cmd/server/main.go", "server/main.go") }, "reference", "docs/structure.md:3", "does not exist"},
		{"line out of range", "docs/request-flow.md", func(s string) string { return strings.ReplaceAll(s, "api.go:12", "api.go:400") }, "reference", "docs/request-flow.md:3", "lines"},
		{"unknown package symbol", "docs/errors.md", func(s string) string { return strings.ReplaceAll(s, "api.writeError", "api.writeProblem") }, "reference", "docs/errors.md:3", "package api declares no writeProblem"},
		{"unknown route", "docs/api.md", func(s string) string { return s + "\nSee `POST /items`.\n" }, "reference", "docs/api.md:13", "no route serves it"},
		{"unknown make target", "docs/testing.md", func(s string) string { return strings.ReplaceAll(s, "make test", "make lint") }, "reference", "docs/testing.md:4", `"lint"`},
		{"unknown Makefile target", "docs/testing.md", func(s string) string { return strings.ReplaceAll(s, "Makefile:test", "Makefile:ship") }, "reference", "docs/testing.md:8", "no target ship"},
		{"unknown env var in code", "docs/testing.md", func(s string) string { return strings.ReplaceAll(s, "PORT=9090", "LOG_LEVEL=debug") }, "reference", "docs/testing.md:5", "LOG_LEVEL is not an environment variable"},
		{"unknown env var in a table", "docs/configuration.md", func(s string) string { return s + "| `TIMEOUT` | 5s |\n" }, "reference", "docs/configuration.md:8", "TIMEOUT"},
		{"unknown bare file name", "docs/structure.md", func(s string) string { return s + "\nSee `server.go`.\n" }, "reference", "docs/structure.md:6", "file server.go does not exist"},
		{"broken link", "docs/overview.md", func(s string) string { return strings.ReplaceAll(s, "(api.md)", "(reference.md)") }, "reference", "docs/overview.md:3", "1 link(s) do not resolve"},
		{"links grouped per page", "docs/overview.md", func(s string) string {
			return s + "[a](internal/api/api.go:3) and [b](internal/api/api.go:9)\n"
		}, "reference", "docs/overview.md:4", "line 4: (internal/api/api.go:3), line 4: (internal/api/api.go:9)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := goodDocs()
			if s := tc.edit(pages[tc.page]); s == "" {
				delete(pages, tc.page)
			} else {
				pages[tc.page] = s
			}
			rep := GateB(docsDir(t, pages))
			if len(rep.Issues) != 1 {
				t.Fatalf("want one issue, got:\n%s", rep)
			}
			is := rep.Issues[0]
			if is.Check != tc.check || is.Where != tc.where || !strings.Contains(is.Message, tc.want) {
				t.Errorf("issue = [%s] %s: %s\nwant [%s] %s containing %q", is.Check, is.Where, is.Message, tc.check, tc.where, tc.want)
			}
		})
	}
}

func TestGateBScope(t *testing.T) {
	in := docsDir(t, goodDocs())
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(in.Dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(in.Dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/api/api.go", docsAPIGo+"// changed\n")
	write("docs/notes.md", "# Notes\n")
	if err := os.Remove(filepath.Join(in.Dir, "go.mod")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, is := range GateB(in).Issues {
		got = append(got, is.Check+" "+is.Where)
	}
	want := []string{"scope docs/notes.md", "scope go.mod", "scope internal/api/api.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("issues = %v, want %v", got, want)
	}
}

// A route registered as "GET /{$}" is named as served, "GET /", or as registered.
func TestExactRoute(t *testing.T) {
	f := docsFacts()
	f.Routes = append(f.Routes, gofacts.Route{Pattern: "GET /{$}", Method: "GET", Path: "/", ExactMatch: true})
	var m Mention
	for _, x := range DocMentions(f)["docs/api.md"] {
		if x.Kind == "route" && x.Text == "GET /" {
			m = x
		}
	}
	if !reflect.DeepEqual(m.Alts, []string{"GET /{$}"}) {
		t.Fatalf("mention = %+v, want GET / with GET /{$} as an alternative", m)
	}
	for _, line := range []string{"## `GET /`", "Registered as `GET /{$}`."} {
		if !parseMarkdown(line).mentions(m) {
			t.Errorf("%q does not count as mentioning the route", line)
		}
	}
	if parseMarkdown("`GET /healthz`").mentions(m) {
		t.Error("GET /healthz counts as mentioning GET /")
	}
	k := newKnown(t.TempDir(), f)
	for _, ref := range []string{"GET /", "GET /{$}", "HEAD /"} {
		method, p, _ := strings.Cut(ref, " ")
		if msg := k.route(method, p); msg != "" {
			t.Errorf("route %s: %s", ref, msg)
		}
	}
}

func TestCodeSpans(t *testing.T) {
	got := codeSpans("a `x` b ``y ` z`` c `unclosed")
	want := []string{"x", "y ` z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codeSpans = %q, want %q", got, want)
	}
}

func TestPathMatches(t *testing.T) {
	for _, tc := range []struct {
		route, p string
		want     bool
	}{
		{"/items/{id}", "/items/7", true},
		{"/items/{id}", "/items/{id}", true},
		{"/items/{id}", "/items", false},
		{"/items/{id}", "/items/7/x", false},
		{"/", "/", true},
		{"/files/{path...}", "/files/a/b", true},
	} {
		if got := pathMatches(tc.route, tc.p); got != tc.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", tc.route, tc.p, got, tc.want)
		}
	}
}
