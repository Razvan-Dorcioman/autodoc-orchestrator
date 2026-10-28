package gofacts

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func extractTestSvc(t *testing.T) *Facts {
	t.Helper()
	f, err := Extract(context.Background(), "testdata/svc")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExtractModule(t *testing.T) {
	f := extractTestSvc(t)
	if f.Module != "example.com/svc" || f.GoVersion != "1.22" {
		t.Errorf("module = %q %q", f.Module, f.GoVersion)
	}
	if len(f.Packages) != 3 || f.Packages[2].Doc != "Package api is the HTTP layer of the test service." {
		t.Errorf("packages = %+v", f.Packages)
	}
}

func TestExtractMains(t *testing.T) {
	f := extractTestSvc(t)
	want := []Main{
		{Package: "example.com/svc/cmd/migrate", File: "cmd/migrate/main.go", Pos: "cmd/migrate/main.go:4"},
		{Package: "example.com/svc/cmd/server", File: "cmd/server/main.go", Pos: "cmd/server/main.go:12", ServesRoutes: true},
	}
	if !reflect.DeepEqual(f.Mains, want) {
		t.Errorf("mains = %+v, want %+v", f.Mains, want)
	}
	if got, err := f.Entrypoint(); got != "cmd/server/main.go" || err != nil {
		t.Errorf("Entrypoint() = %q, %v", got, err)
	}
}

func TestEntrypoint(t *testing.T) {
	serving := func(files ...string) *Facts {
		f := &Facts{Mains: []Main{{File: "cmd/tool/main.go"}}}
		for _, file := range files {
			f.Mains = append(f.Mains, Main{File: file, ServesRoutes: true})
		}
		return f
	}
	if got, err := serving("cmd/api/main.go").Entrypoint(); got != "cmd/api/main.go" || err != nil {
		t.Errorf("one serving main: %q, %v", got, err)
	}
	if _, err := serving().Entrypoint(); err == nil || !strings.Contains(err.Error(), "--main") {
		t.Errorf("no serving main: err = %v, want one naming --main", err)
	}
	if _, err := serving("cmd/a/main.go", "cmd/b/main.go").Entrypoint(); err == nil || !strings.Contains(err.Error(), "cmd/a/main.go, cmd/b/main.go") {
		t.Errorf("two serving mains: err = %v, want one listing both", err)
	}
}

func TestExtractRoutes(t *testing.T) {
	f := extractTestSvc(t)
	if len(f.Routes) != 6 {
		t.Fatalf("routes = %+v, want 6", f.Routes)
	}

	idx := f.Routes[0]
	if idx.Method != "GET" || idx.Path != "/" || !idx.ExactMatch || idx.Handler != "api.index" {
		t.Errorf("index route = %+v", idx)
	}
	want := []Param{{
		Name: "limit", In: "query", Pos: "internal/api/api.go:54",
		Default: &Constraint{Value: "5", Pos: "internal/api/api.go:53"},
		Maximum: &Constraint{Value: "20", Pos: "internal/api/api.go:56"},
	}}
	if !reflect.DeepEqual(idx.Params, want) {
		t.Errorf("index params = %+v, want %+v", idx.Params, want)
	}
	if want := []string{"api.defaultLimit", "api.maxLimit"}; !reflect.DeepEqual(idx.ConstsUsed, want) {
		t.Errorf("index consts = %v, want %v", idx.ConstsUsed, want)
	}
	if got := statuses(idx.Responses); !reflect.DeepEqual(got, []int{400, 200}) {
		t.Errorf("index statuses = %v", got)
	}
	bad := idx.Responses[0]
	if bad.Code != "invalid_limit" || bad.Message != "limit must be at most %d" || bad.Body != "api.ErrorResponse" || bad.Helper != "writeError" {
		t.Errorf("index 400 = %+v", bad)
	}
	if ok := idx.Responses[1]; ok.Body != "api.ItemsResponse" || ok.StatusName != "StatusOK" {
		t.Errorf("index 200 = %+v", ok)
	}

	it := f.Routes[1]
	if it.Path != "/items/{id}" || it.ExactMatch {
		t.Errorf("item route = %+v", it)
	}
	if got := paramNames(it.Params); !reflect.DeepEqual(got, []string{"path:id", "query:color"}) {
		t.Errorf("item params = %v (Has and Get on the same name count once)", got)
	}
	if got := statuses(it.Responses); !reflect.DeepEqual(got, []int{400, 400, 404, 200}) {
		t.Errorf("item statuses = %v", got)
	}

	files := f.Routes[2]
	if files.Method != "POST" || files.Path != "/files/{path}" || files.Handler != "http.NotFoundHandler()" || files.HandlerPos != "" {
		t.Errorf("files route = %+v", files)
	}
	if idx.Body != nil || it.Body != nil {
		t.Errorf("GET routes have bodies: %+v, %+v", idx.Body, it.Body)
	}
}

// TestExtractMethodlessRoutes covers registrations without a method: a
// switch on r.Method that hands each method to a function becomes one route
// per method; a handler that checks r.Method itself keeps one route with the
// methods it checks.
func TestExtractMethodlessRoutes(t *testing.T) {
	f := extractTestSvc(t)
	list, create, notes := f.Routes[3], f.Routes[4], f.Routes[5]

	if list.Pattern != "GET /orders" || list.Method != "GET" || list.Handler != "api.listOrders" || list.Dispatcher != "api.orders" ||
		list.Registered != "internal/api/api.go:46" || list.Body != nil {
		t.Errorf("GET /orders = %+v", list)
	}
	if got := statuses(list.Responses); !reflect.DeepEqual(got, []int{200}) || list.Responses[0].Body != "[]api.Order" {
		t.Errorf("GET /orders responses = %+v (the dispatcher's 405 belongs to no route)", list.Responses)
	}

	if create.Pattern != "POST /orders" || create.Handler != "api.createOrder" || create.HandlerPos != "internal/api/orders.go:47" {
		t.Errorf("POST /orders = %+v", create)
	}
	if want := (&RequestBody{Type: "api.CreateOrder", Pos: "internal/api/orders.go:49", Via: "api.decode"}); !reflect.DeepEqual(create.Body, want) {
		t.Errorf("POST /orders body = %+v, want %+v", create.Body, want)
	}
	if got := statuses(create.Responses); !reflect.DeepEqual(got, []int{400, 422, 201}) {
		t.Errorf("POST /orders statuses = %v", got)
	}
	if bad := create.Responses[1]; bad.Code != "invalid_qty" || bad.Body != "api.ErrorResponse" {
		t.Errorf("POST /orders 422 = %+v: err.Error() is the message, not the body", bad)
	}

	if notes.Pattern != "/notes" || notes.Method != "" || !reflect.DeepEqual(notes.Methods, []string{"PUT"}) || notes.Handler != "api.notes" {
		t.Errorf("/notes = %+v", notes)
	}
	if !reflect.DeepEqual(notes.OpMethods(), []string{"PUT"}) || !reflect.DeepEqual(list.OpMethods(), []string{"GET"}) {
		t.Errorf("OpMethods = %v, %v", notes.OpMethods(), list.OpMethods())
	}
	if want := (&RequestBody{Type: "api.Note", Pos: "internal/api/orders.go:80"}); !reflect.DeepEqual(notes.Body, want) {
		t.Errorf("/notes body = %+v, want %+v (json.Unmarshal of the read body)", notes.Body, want)
	}
}

func TestExtractReach(t *testing.T) {
	f := extractTestSvc(t)
	got := map[string]int{}
	for _, c := range f.Constants {
		got[c.Name] = c.Reach
	}
	for _, fn := range f.Functions {
		got[fn.Name] = fn.Reach
	}
	for name, want := range map[string]int{
		"defaultLimit": 1, // used by a handler
		"Red":          1, // its type is: item converts to Color
		"maxQty":       2, // used by checkQty, which createOrder calls
		"orders":       1, // a dispatcher
		"createOrder":  1, // a method's handler
		"decode":       2,
		"NewRouter":    0, // registers the routes but no handler calls it
		"recoverer":    0,
	} {
		if got[name] != want {
			t.Errorf("reach of %s = %d, want %d", name, got[name], want)
		}
	}
}

func TestExtractMiddlewareResponses(t *testing.T) {
	f := extractTestSvc(t)
	var found bool
	for _, s := range f.Responses {
		if s.Func == "api.recoverer" && s.Status == 500 && s.Code == "internal" && s.Body == "api.ErrorResponse" {
			found = true
		}
	}
	if !found {
		t.Errorf("no 500 from recoverer in %+v", f.Responses)
	}
	if got := len(f.ImplicitResponses); got != 3 {
		t.Errorf("implicit responses = %+v, want 404, 405 and HEAD", f.ImplicitResponses)
	}
}

func TestExtractDecls(t *testing.T) {
	f := extractTestSvc(t)
	consts := map[string]Constant{}
	for _, c := range f.Constants {
		consts[c.Name] = c
	}
	if c := consts["maxLimit"]; c.Value != "20" || c.Type != "untyped int" {
		t.Errorf("maxLimit = %+v", c)
	}
	if c := consts["Red"]; c.Value != "red" || c.Type != "api.Color" {
		t.Errorf("Red = %+v", c)
	}
	if len(f.Vars) != 1 || f.Vars[0].Name != "weights" || f.Vars[0].Init != "map[Color]int{Red: 1, Blue: 2}" {
		t.Errorf("vars = %+v", f.Vars)
	}

	var item Type
	for _, ty := range f.Types {
		if ty.Name == "Item" {
			item = ty
		}
	}
	want := Field{Name: "Color", Type: "string", JSON: "color", OmitEmpty: true, Doc: "empty when unknown"}
	if item.Doc != "Item is one entry." || len(item.Fields) != 3 || item.Fields[2] != want {
		t.Errorf("Item = %+v", item)
	}
}

func TestExtractConfigAndBuild(t *testing.T) {
	f := extractTestSvc(t)
	wantEnv := []EnvVar{
		{Name: "PORT", Default: "8080", Via: "envOr", Pos: "cmd/server/main.go:14"},
		{Name: "DEBUG", Pos: "cmd/server/main.go:18"},
	}
	if !reflect.DeepEqual(f.Env, wantEnv) {
		t.Errorf("env = %+v, want %+v", f.Env, wantEnv)
	}
	if len(f.ServerSettings) != 3 || f.ServerSettings[2] != (Setting{Name: "http.Server.ReadTimeout", Value: "10 * time.Second", Pos: "cmd/server/main.go:16"}) {
		t.Errorf("server settings = %+v", f.ServerSettings)
	}
	wantMake := []MakeTarget{
		{Name: "run", Recipe: []string{"go run ./cmd/server"}},
		{Name: "test", Deps: []string{"vet"}, Recipe: []string{"go test ./..."}},
	}
	if !reflect.DeepEqual(f.MakeTargets, wantMake) {
		t.Errorf("make = %+v, want %+v", f.MakeTargets, wantMake)
	}
	if !reflect.DeepEqual(f.Dockerfile, []string{"FROM golang:1.22 AS build", "EXPOSE 8080"}) {
		t.Errorf("dockerfile = %q", f.Dockerfile)
	}
	if !reflect.DeepEqual(f.Tests, []Test{{Name: "TestIndex", Package: "api", Pos: "internal/api/api_test.go:5"}}) {
		t.Errorf("tests = %+v", f.Tests)
	}
}

func TestParsePattern(t *testing.T) {
	for _, tt := range []struct {
		in         string
		method     string
		path       string
		exactMatch bool
	}{
		{"GET /{$}", "GET", "/", true},
		{"/static/", "", "/static/", false},
		{"POST example.com/a/{b}", "POST", "/a/{b}", false},
		{"GET /files/{rest...}", "GET", "/files/{rest}", false},
	} {
		r := parsePattern(tt.in)
		if r.Method != tt.method || r.Path != tt.path || r.ExactMatch != tt.exactMatch {
			t.Errorf("parsePattern(%q) = %+v", tt.in, r)
		}
	}
}

func statuses(sites []ResponseSite) []int {
	var out []int
	for _, s := range sites {
		out = append(out, s.Status)
	}
	return out
}

func paramNames(ps []Param) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.In+":"+p.Name)
	}
	return out
}
