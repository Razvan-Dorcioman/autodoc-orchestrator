package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"autodoc/internal/strip"
)

const testSpec = `{
  "swagger": "2.0",
  "info": {"title": "Catalog", "version": "1.0"},
  "basePath": "/",
  "paths": {
    "/healthz": {"get": {"responses": {"200": {"schema": {"$ref": "#/definitions/httpapi.HealthResponse"}}}}},
    "/products/{id}": {"get": {
      "parameters": [{"name": "id", "in": "path", "required": true}],
      "responses": {
        "200": {"schema": {"$ref": "#/definitions/httpapi.ProductResponse"}},
        "404": {"description": "product_not_found", "schema": {"$ref": "#/definitions/httpapi.ErrorResponse"}}
      }
    }}
  },
  "definitions": {
    "httpapi.HealthResponse": {"properties": {"status": {"type": "string"}}},
    "httpapi.ProductResponse": {"properties": {"price": {"$ref": "#/definitions/httpapi.Money"}}},
    "httpapi.Money": {"properties": {"amount": {"type": "number"}}},
    "httpapi.ErrorResponse": {"properties": {"error": {"$ref": "#/definitions/httpapi.ErrorBody"}}},
    "httpapi.ErrorBody": {"properties": {"code": {"type": "string"}}}
  }
}`

func loadTestSpec(t *testing.T) map[string]any {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(strip.SpecDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "swagger.json"), []byte(testSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := LoadSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestGetSpec(t *testing.T) {
	doc := loadTestSpec(t)

	got, err := GetSpec(doc, GetSpecArgs{Path: "/products/{id}", Method: "GET"})
	if err != nil {
		t.Fatalf("GetSpec: %v", err)
	}
	if len(got.Operations) != 1 || got.Operations[0].Method != "get" || got.Operations[0].Path != "/products/{id}" {
		t.Fatalf("operations = %+v", got.Operations)
	}
	var names []string
	for name := range got.Definitions {
		names = append(names, name)
	}
	want := map[string]bool{"httpapi.ProductResponse": true, "httpapi.Money": true, "httpapi.ErrorResponse": true, "httpapi.ErrorBody": true}
	if len(names) != len(want) {
		t.Errorf("definitions = %v, want the four the operation references, transitively", names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("definition %s is not referenced by the operation", n)
		}
	}
	if got.Info["title"] != "Catalog" {
		t.Errorf("info = %v", got.Info)
	}
	// The result goes through encoding/json on its way to the model.
	if _, err := json.Marshal(got); err != nil {
		t.Error(err)
	}

	all, err := GetSpec(doc, GetSpecArgs{})
	if err != nil || len(all.Operations) != 2 || len(all.Definitions) != 5 {
		t.Errorf("GetSpec(all) = %d operations, %d definitions, err %v; want 2, 5", len(all.Operations), len(all.Definitions), err)
	}
}

func TestGetSpecNotFound(t *testing.T) {
	doc := loadTestSpec(t)
	for _, args := range []GetSpecArgs{{Path: "/orders"}, {Path: "/healthz", Method: "post"}} {
		if _, err := GetSpec(doc, args); err == nil {
			t.Errorf("GetSpec(%+v) succeeded, want error", args)
		}
	}
}
