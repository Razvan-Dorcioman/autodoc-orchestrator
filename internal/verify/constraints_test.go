package verify

import (
	"encoding/json"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
)

// constrainedFacts is catalogFacts with the constraints the extractor reads
// from the catalog's product handler.
func constrainedFacts() *gofacts.Facts {
	f := catalogFacts()
	for i := range f.Routes {
		for j, p := range f.Routes[i].Params {
			switch p.Name {
			case "id":
				p.Minimum = &gofacts.Constraint{Value: "1", Pos: "h.go:47"}
			case "currency":
				p.Default = &gofacts.Constraint{Value: "RON", Pos: "h.go:54"}
				p.Enum = &gofacts.EnumConstraint{Values: []string{"RON", "EUR", "HUF"}, Pos: "h.go:56", Via: "catalog.ParseCurrency returns a catalog.Currency"}
			}
			f.Routes[i].Params[j] = p
		}
	}
	return f
}

func TestCheckConstraints(t *testing.T) {
	params := func(s map[string]any) []any {
		return at(s, "paths", "/products/{id}", "get")["parameters"].([]any)
	}
	param := func(s map[string]any, name string) map[string]any {
		for _, p := range params(s) {
			if p := p.(map[string]any); p["name"] == name {
				return p
			}
		}
		panic(name)
	}
	complete := func(s map[string]any) {
		param(s, "id")["minimum"] = 1
		c := param(s, "currency")
		c["default"], c["enum"] = "RON", []any{"EUR", "HUF", "RON"}
	}
	tests := []struct {
		name   string
		mutate func(s map[string]any)
		want   []string // a part of each issue message, in order
	}{
		{name: "all constraints in the spec, enum in another order", mutate: complete},
		{name: "none in the spec", mutate: func(map[string]any) {}, want: []string{
			"path param id: missing minimum(1): the handler rejects values below 1 (h.go:47); add it to its @Param line",
			"query param currency: missing default(RON)",
			"query param currency: missing Enums(RON,EUR,HUF): the handler accepts only RON, EUR, HUF: catalog.ParseCurrency",
		}},
		{name: "contradicted", mutate: func(s map[string]any) {
			complete(s)
			param(s, "id")["minimum"] = 0
			c := param(s, "currency")
			c["default"], c["enum"] = "EUR", []any{"RON", "EUR"}
		}, want: []string{
			"path param id: minimum is 0 in the spec, but the handler rejects values below 1 (h.go:47); set minimum(1)",
			"query param currency: default is EUR in the spec, but the handler uses RON when it is absent",
			"query param currency: the spec allows RON, EUR, but the handler accepts only RON, EUR, HUF",
		}},
		{name: "a constraint the facts do not know is left alone", mutate: func(s map[string]any) {
			complete(s)
			param(s, "id")["maximum"] = 1000
		}},
		{name: "a param missing from the spec is checkSpec's", mutate: func(s map[string]any) {
			complete(s)
			op := at(s, "paths", "/products/{id}", "get")
			op["parameters"] = params(s)[:1]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goodSpec()
			tt.mutate(s)
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			got := checkConstraints(data, constrainedFacts())
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues, want %d:\n%s", len(got), len(tt.want), Report{Gate: "A", Issues: got})
			}
			for i, is := range got {
				if is.Check != ConstraintCheck || !strings.Contains(is.Where, "GET /products/{id}") {
					t.Errorf("issue %d = %+v, want a constraint issue on the product route", i, is)
				}
				if !strings.Contains(is.Message, tt.want[i]) {
					t.Errorf("issue %d = %q, want it to contain %q", i, is.Message, tt.want[i])
				}
			}
		})
	}
}

func TestReportBlocking(t *testing.T) {
	rep := Report{Gate: "A", Issues: []Issue{{Check: ConstraintCheck}, {Check: "spec"}, {Check: ConstraintCheck}}}
	if b := rep.Blocking(); len(b) != 1 || b[0].Check != "spec" {
		t.Errorf("Blocking() = %+v, want the spec issue only", b)
	}
	if len(rep.Issues) != 3 {
		t.Errorf("Blocking() changed the report: %+v", rep.Issues)
	}
	if b := (Report{Issues: []Issue{{Check: ConstraintCheck}}}).Blocking(); len(b) != 0 {
		t.Errorf("constraint issues alone: Blocking() = %+v, want none", b)
	}
}
