package docs

import (
	"fmt"
	"path"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/verify"
)

func TestPageMessages(t *testing.T) {
	f := &gofacts.Facts{
		Module: "svc",
		Routes: []gofacts.Route{{Pattern: "GET /items/{id}", Method: "GET", Path: "/items/{id}", Handler: "api.(*handlers).item",
			Responses: []gofacts.ResponseSite{{Status: 404, Code: "item_not_found", Body: "api.ErrorBody"}}}},
		Responses: []gofacts.ResponseSite{{Status: 404, Code: "item_not_found", Body: "api.ErrorBody"}},
		Env:       []gofacts.EnvVar{{Name: "PORT"}},
	}
	spec := map[string]any{"paths": map[string]any{"/items/{id}": map[string]any{"get": map[string]any{"summary": "Get an item"}}}}
	a := &Agent{facts: f, spec: spec, mentions: verify.DocMentions(f)}
	for _, p := range verify.DocPages {
		msg, err := a.pageMessage(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !strings.Contains(msg, `path "`+p+`"`) {
			t.Errorf("%s: message does not name the page to write", p)
		}
		for _, m := range a.mentions[p] {
			if !strings.Contains(msg, "`"+m.Text+"`") {
				t.Errorf("%s: message does not list required mention %s", p, m.Text)
			}
		}
		if strings.Contains(msg, "]("+path.Base(p)+")") {
			t.Errorf("%s: message lists the page among the other pages", p)
		}
	}
	if msg, _ := a.pageMessage("docs/api.md"); !strings.Contains(msg, "Get an item") {
		t.Error("api.md message does not carry the spec")
	}
}

// TestBriefsAreBounded: a large module's function list keeps the functions
// the handlers reach, nearest first, and says how many it left out.
func TestBriefsAreBounded(t *testing.T) {
	f := &gofacts.Facts{}
	for i := range maxFunctionBriefs + 5 {
		f.Functions = append(f.Functions, gofacts.Function{Name: fmt.Sprintf("unused%d", i), Package: "util"})
	}
	f.Functions = append(f.Functions,
		gofacts.Function{Name: "price", Package: "shop", Reach: 2},
		gofacts.Function{Name: "handle", Package: "api", Reach: 1})

	got, ok := functionBriefs(f).(map[string]any)
	if !ok {
		t.Fatalf("functionBriefs = %T, want a listing with an omitted note", functionBriefs(f))
	}
	listed := got["listed"].([]map[string]any)
	if len(listed) != maxFunctionBriefs || listed[0]["name"] != "api.handle" || listed[1]["name"] != "shop.price" {
		t.Errorf("listed %d, first %v, %v", len(listed), listed[0]["name"], listed[1]["name"])
	}
	if note := got["omitted"].(string); !strings.HasPrefix(note, "7 more functions") {
		t.Errorf("omitted = %q", note)
	}
	if _, ok := functionBriefs(&gofacts.Facts{Functions: f.Functions[:3]}).([]map[string]any); !ok {
		t.Error("a short list should be returned as is")
	}
}
