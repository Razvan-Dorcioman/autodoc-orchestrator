package docs

import (
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
