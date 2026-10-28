package verify

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
)

const origGo = "package a\n\n// f does things.\nfunc f() int { return 1 }\n"

// snapshotOf writes files into a fresh dir and fingerprints it.
func snapshotOf(t *testing.T, files map[string]string) (string, guard.Snapshot) {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := guard.Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, snap
}

func TestCheckCodeAndScope(t *testing.T) {
	baseFiles := map[string]string{"go.mod": "module a\n", "a.go": origGo, "README.md": "# a\n"}
	_, base := snapshotOf(t, baseFiles)
	original := func(rel string) ([]byte, error) {
		if s, ok := baseFiles[rel]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("not in base")
	}

	tests := []struct {
		name    string
		changes map[string]string // overrides of the base files; "" deletes
		want    []string          // "check where"
	}{
		{name: "unchanged"},
		{name: "annotations and spec files", changes: map[string]string{
			"a.go":                          "package a\n\n// f does things.\n// @Summary F\n// @Router /f [get]\nfunc f() int { return 1 }\n",
			"internal/apispec/swagger.json": "{}",
		}},
		{name: "code change", changes: map[string]string{"a.go": "package a\n\nfunc f() int { return 2 }\n"}, want: []string{"code a.go"}},
		{name: "existing comment edited", changes: map[string]string{"a.go": "package a\n\n// f does other things.\nfunc f() int { return 1 }\n"}, want: []string{"scope a.go"}},
		{name: "layout changed", changes: map[string]string{"a.go": "package a\n\n// f does things.\nfunc f() int {\n\treturn 1\n}\n"}, want: []string{"scope a.go"}},
		{name: "dependency added", changes: map[string]string{"go.mod": "module a\n\nrequire x v1\n", "go.sum": "x v1 h1:\n"}, want: []string{"code go.mod", "code go.sum"}},
		{name: "other files", changes: map[string]string{"README.md": "# b\n", "docs/new.md": "x", "a.go": ""}, want: []string{"scope README.md", "scope a.go", "scope docs/new.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range baseFiles {
				files[k] = v
			}
			for k, v := range tt.changes {
				if v == "" {
					delete(files, k)
				} else {
					files[k] = v
				}
			}
			dir, cur := snapshotOf(t, files)
			var got []string
			for _, is := range append(checkCode(base, cur), checkScope(base, cur, dir, original)...) {
				got = append(got, is.Check+" "+is.Where)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("issues = %v, want %v", got, tt.want)
			}
		})
	}
}

func catalogFacts() *gofacts.Facts {
	errSite := func(status int, code string) gofacts.ResponseSite {
		return gofacts.ResponseSite{Status: status, Code: code, Body: "httpapi.ErrorResponse", Pos: "h.go:1"}
	}
	return &gofacts.Facts{
		Routes: []gofacts.Route{
			{Pattern: "GET /{$}", Method: "GET", Path: "/", Handler: "httpapi.(*handlers).index",
				Params:    []gofacts.Param{},
				Responses: []gofacts.ResponseSite{{Status: 200, Body: "httpapi.HealthResponse"}}},
			{Pattern: "GET /products/{id}", Method: "GET", Path: "/products/{id}", Handler: "httpapi.(*handlers).product",
				Params: []gofacts.Param{{Name: "id", In: "path"}, {Name: "currency", In: "query"}},
				Responses: []gofacts.ResponseSite{
					errSite(400, "invalid_id"), errSite(400, "invalid_currency"), errSite(404, "product_not_found"),
					{Status: 200, Body: "httpapi.HealthResponse"},
				}},
		},
		Types: []gofacts.Type{
			{Name: "HealthResponse", Package: "httpapi", Underlying: "struct", Fields: []gofacts.Field{{Name: "Status", Type: "string", JSON: "status"}}},
			{Name: "ErrorResponse", Package: "httpapi", Underlying: "struct", Fields: []gofacts.Field{{Name: "Error", Type: "ErrorBody", JSON: "error"}, {Name: "internal", Type: "int"}}},
		},
	}
}

// goodSpec matches catalogFacts.
func goodSpec() map[string]any {
	ref := func(name string) map[string]any { return map[string]any{"$ref": "#/definitions/httpapi." + name} }
	return map[string]any{
		"swagger":  "2.0",
		"info":     map[string]any{"title": "Catalog", "version": "1.0"},
		"basePath": "/",
		"paths": map[string]any{
			"/": map[string]any{"get": map[string]any{
				"responses": map[string]any{"200": map[string]any{"description": "OK", "schema": ref("HealthResponse")}},
			}},
			"/products/{id}": map[string]any{"get": map[string]any{
				"parameters": []any{
					map[string]any{"name": "id", "in": "path", "required": true, "type": "integer"},
					map[string]any{"name": "currency", "in": "query", "type": "string"},
				},
				"responses": map[string]any{
					"200": map[string]any{"description": "OK", "schema": ref("HealthResponse")},
					"400": map[string]any{"description": "invalid_id or invalid_currency", "schema": ref("ErrorResponse")},
					"404": map[string]any{"description": "product_not_found", "schema": ref("ErrorResponse")},
				},
			}},
		},
		"definitions": map[string]any{
			"httpapi.HealthResponse": map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}}},
			"httpapi.ErrorResponse":  map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "object"}}},
		},
	}
}

// at walks a decoded spec by keys.
func at(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func TestCheckSpec(t *testing.T) {
	product := func(s map[string]any) map[string]any { return at(s, "paths", "/products/{id}", "get") }
	tests := []struct {
		name   string
		mutate func(s map[string]any)
		want   []string // a part of each issue message, in order
	}{
		{name: "matches the facts", mutate: func(map[string]any) {}},
		{name: "missing route", mutate: func(s map[string]any) { delete(at(s, "paths"), "/") },
			want: []string{"route is not in the spec: annotate the handler with @Router / [get]"}},
		{name: "invented route and method", mutate: func(s map[string]any) {
			at(s, "paths")["/admin"] = map[string]any{"get": map[string]any{}}
			at(s, "paths", "/")["post"] = map[string]any{}
		}, want: []string{"get /admin: operation is not a route", "post /: operation is not a route"}},
		{name: "basePath is part of the served path", mutate: func(s map[string]any) { s["basePath"] = "/api" },
			want: []string{"GET /{$}", "GET /products/{id}", "get /api", "get /api/products/{id}"}},
		{name: "params", mutate: func(s map[string]any) {
			product(s)["parameters"] = []any{
				map[string]any{"name": "id", "in": "path", "required": false},
				map[string]any{"name": "limit", "in": "query"},
			}
		}, want: []string{"path param id must be required", "@Param limit (in query) is not read", "missing @Param currency query"}},
		{name: "statuses", mutate: func(s map[string]any) {
			r := at(product(s), "responses")
			delete(r, "404")
			r["500"] = map[string]any{"description": "boom"}
		}, want: []string{"response 500 is documented but the handler never writes it", "status 404 is not documented"}},
		{name: "error codes and body", mutate: func(s map[string]any) {
			at(product(s), "responses")["400"] = map[string]any{"description": "bad request", "schema": map[string]any{"$ref": "#/definitions/httpapi.HealthResponse"}}
		}, want: []string{"response 400 schema is httpapi.HealthResponse; the handler writes {object} httpapi.ErrorResponse", `name error code "invalid_id"`, `name error code "invalid_currency"`}},
		{name: "definition fields", mutate: func(s map[string]any) {
			at(s, "definitions", "httpapi.HealthResponse")["properties"] = map[string]any{"state": map[string]any{}}
		}, want: []string{`missing property "status"`, `property "state" is not a JSON field`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goodSpec()
			tt.mutate(s)
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			got := checkSpec(data, catalogFacts())
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues, want %d:\n%s", len(got), len(tt.want), Report{Gate: "A", Issues: got})
			}
			for i, is := range got {
				if text := is.Where + ": " + is.Message; !strings.Contains(text, tt.want[i]) {
					t.Errorf("issue %d = %q, want it to contain %q", i, text, tt.want[i])
				}
			}
		})
	}
}

// TestCheckSpecMethodsAndBodies covers routes registered without a method
// and handlers that decode a request body.
func TestCheckSpecMethodsAndBodies(t *testing.T) {
	ok := func(status int, body string) gofacts.ResponseSite {
		return gofacts.ResponseSite{Status: status, Body: body, Pos: "h.go:1"}
	}
	facts := &gofacts.Facts{Routes: []gofacts.Route{
		{Pattern: "GET /orders", Method: "GET", Path: "/orders", Handler: "api.listOrders", Responses: []gofacts.ResponseSite{ok(200, "[]api.Order")}},
		{Pattern: "POST /orders", Method: "POST", Path: "/orders", Handler: "api.createOrder", Responses: []gofacts.ResponseSite{ok(201, "api.Order")},
			Body: &gofacts.RequestBody{Type: "api.CreateOrder", Pos: "h.go:5"}},
		{Pattern: "/notes", Methods: []string{"PUT"}, Path: "/notes", Handler: "api.notes", Responses: []gofacts.ResponseSite{ok(200, "api.Note")},
			Body: &gofacts.RequestBody{Type: "api.Note", Pos: "h.go:9"}},
		{Pattern: "/ping", Path: "/ping", Handler: "api.ping", Responses: []gofacts.ResponseSite{ok(200, "map[string]string")}},
	}}
	ref := func(name string) map[string]any { return map[string]any{"$ref": "#/definitions/api." + name} }
	body := func(name string) []any {
		return []any{map[string]any{"name": "body", "in": "body", "required": true, "schema": ref(name)}}
	}
	resp := func(status string, schema any) map[string]any {
		return map[string]any{status: map[string]any{"description": "OK", "schema": schema}}
	}
	good := func() map[string]any {
		return map[string]any{"swagger": "2.0", "basePath": "/", "paths": map[string]any{
			"/orders": map[string]any{
				"get":  map[string]any{"responses": resp("200", map[string]any{"type": "array", "items": ref("Order")})},
				"post": map[string]any{"parameters": body("CreateOrder"), "responses": resp("201", ref("Order"))},
			},
			"/notes": map[string]any{"put": map[string]any{"parameters": body("Note"), "responses": resp("200", ref("Note"))}},
			"/ping": map[string]any{
				"get":  map[string]any{"responses": resp("200", map[string]any{"type": "object"})},
				"head": map[string]any{"responses": resp("200", map[string]any{"type": "object"})},
			},
		}}
	}
	tests := []struct {
		name   string
		mutate func(s map[string]any)
		want   []string
	}{
		{name: "matches the facts", mutate: func(map[string]any) {}},
		{name: "a checked method is missing", mutate: func(s map[string]any) {
			delete(at(s, "paths"), "/notes")
		}, want: []string{"/notes (handler api.notes at ): route is not in the spec: annotate the handler with @Router /notes [put]"}},
		{name: "an unchecked method is invented", mutate: func(s map[string]any) {
			at(s, "paths", "/notes")["post"] = map[string]any{}
		}, want: []string{"post /notes: operation is not a route"}},
		{name: "a method-blind route needs one operation", mutate: func(s map[string]any) {
			delete(at(s, "paths"), "/ping")
		}, want: []string{"/ping (handler api.ping at ): route is not in the spec: the handler does not check r.Method"}},
		{name: "a method-blind route checks each operation", mutate: func(s map[string]any) {
			at(s, "paths", "/ping", "head")["responses"] = resp("204", nil)
		}, want: []string{"/ping (handler api.ping at ) HEAD: response 204 is documented", "/ping (handler api.ping at ) HEAD: status 200 is not documented"}},
		{name: "array response", mutate: func(s map[string]any) {
			at(s, "paths", "/orders", "get")["responses"] = resp("200", ref("Order"))
		}, want: []string{"response 200 schema is not an array; the handler writes {array} api.Order"}},
		{name: "missing body", mutate: func(s map[string]any) {
			delete(at(s, "paths", "/orders", "post"), "parameters")
		}, want: []string{"missing @Param body body api.CreateOrder true: the handler decodes the request body at h.go:5"}},
		{name: "wrong body", mutate: func(s map[string]any) {
			at(s, "paths", "/orders", "post")["parameters"] = body("Order")
		}, want: []string{"body param body is api.Order; the handler decodes api.CreateOrder at h.go:5"}},
		{name: "invented body", mutate: func(s map[string]any) {
			at(s, "paths", "/orders", "get")["parameters"] = body("Order")
		}, want: []string{"@Param body (in body) is not read by the handler: it decodes no request body"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good()
			tt.mutate(s)
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			got := checkSpec(data, facts)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues, want %d:\n%s", len(got), len(tt.want), Report{Gate: "A", Issues: got})
			}
			for i, is := range got {
				if text := is.Where + ": " + is.Message; !strings.Contains(text, tt.want[i]) {
					t.Errorf("issue %d = %q, want it to contain %q", i, text, tt.want[i])
				}
			}
		})
	}
}

func TestValidateSpec(t *testing.T) {
	good, _ := json.Marshal(goodSpec())
	tests := []struct {
		name       string
		json, yaml string
		want       []string
	}{
		{name: "valid", json: string(good), yaml: string(good)}, // JSON is valid YAML
		{name: "not JSON", json: "{", yaml: "{}", want: []string{"not valid JSON"}},
		{name: "wrong version, no info, no paths", json: `{"swagger":"3.0","info":{}}`, yaml: `{"swagger":"3.0","info":{}}`,
			want: []string{`swagger is "3.0"`, "info.title and info.version are required", "no paths"}},
		{name: "dangling ref", json: `{"swagger":"2.0","info":{"title":"t","version":"1"},"paths":{"/":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/x.Y"}}}}}}}`,
			yaml: `{"swagger":"2.0","info":{"title":"t","version":"1"},"paths":{"/":{"get":{"responses":{"200":{"schema":{"$ref":"#/definitions/x.Y"}}}}}}}`,
			want: []string{`$ref "#/definitions/x.Y" does not resolve`}},
		{name: "yaml differs", json: string(good), yaml: "swagger: \"2.0\"\n", want: []string{"does not describe the same document"}},
		{name: "yaml invalid", json: string(good), yaml: "a: [", want: []string{"not valid YAML"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateSpec([]byte(tt.json), []byte(tt.yaml))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues, want %d:\n%s", len(got), len(tt.want), Report{Gate: "A", Issues: got})
			}
			for i, is := range got {
				if !strings.Contains(is.Message, tt.want[i]) {
					t.Errorf("issue %d = %q, want it to contain %q", i, is.Message, tt.want[i])
				}
			}
		})
	}
}

func TestReportString(t *testing.T) {
	r := Report{Gate: "A", Issues: []Issue{{Check: "go", Where: "go test ./...", Message: "FAIL\nx_test.go:1"}}}
	want := "gate A failed with 1 issue(s):\n1. [go] go test ./...: FAIL\n   x_test.go:1\n"
	if got := r.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := (Report{Gate: "A"}).String(); got != "gate A passed" {
		t.Errorf("String() = %q", got)
	}
}

func TestRegressions(t *testing.T) {
	base := []CheckResult{{Name: "build", OK: true}, {Name: "vet", OK: false}}
	cur := []CheckResult{
		{Name: "build", OK: false},                // green before: blocks
		{Name: "vet", OK: false},                  // already red before: does not
		{Name: "test", OK: false, Advisory: true}, // the target's own tests: never
	}
	var got []string
	for _, c := range Regressions(base, cur) {
		got = append(got, c.Name)
	}
	if !reflect.DeepEqual(got, []string{"build"}) {
		t.Errorf("Regressions = %v, want [build]", got)
	}
}
