package tools

import (
	"reflect"
	"testing"

	"autodoc/internal/gofacts"
)

func TestGetFacts(t *testing.T) {
	f := &gofacts.Facts{
		Routes: []gofacts.Route{
			{Pattern: "GET /{$}", Path: "/", Handler: "httpapi.(*handlers).index"},
			{Pattern: "GET /products/{id}", Path: "/products/{id}", Handler: "httpapi.(*handlers).product"},
		},
		Types: []gofacts.Type{{Name: "ErrorResponse", Package: "httpapi"}, {Name: "Money", Package: "httpapi"}},
		Constants: []gofacts.Constant{
			{Name: "RON", Package: "catalog", Type: "catalog.Currency"},
			{Name: "EUR", Package: "catalog", Type: "catalog.Currency"},
			{Name: "maxQty", Package: "httpapi", Type: "untyped int"},
		},
		Functions: []gofacts.Function{{Name: "(*Store).Get", Package: "catalog"}, {Name: "ParseCurrency", Package: "catalog"}},
	}
	names := func(r GetFactsResult) []string {
		var out []string
		if r.Route != nil {
			out = append(out, r.Route.Pattern)
		}
		for _, x := range r.Routes {
			out = append(out, x.Pattern)
		}
		for _, x := range r.Types {
			out = append(out, x.Name)
		}
		for _, x := range r.Constants {
			out = append(out, x.Name)
		}
		for _, x := range r.Functions {
			out = append(out, x.Name)
		}
		return append(out, r.NotFound...)
	}
	tests := []struct {
		name string
		args GetFactsArgs
		want []string
	}{
		{"route by pattern", GetFactsArgs{Route: "GET /products/{id}"}, []string{"GET /products/{id}"}},
		{"route by path", GetFactsArgs{Route: "/"}, []string{"GET /{$}"}},
		{"route by short handler", GetFactsArgs{Route: "(*handlers).product"}, []string{"GET /products/{id}"}},
		{"all routes", GetFactsArgs{Route: "*"}, []string{"GET /{$}", "GET /products/{id}"}},
		{"types", GetFactsArgs{Types: []string{"httpapi.ErrorResponse", "Money"}}, []string{"ErrorResponse", "Money"}},
		{"constants by type", GetFactsArgs{Constants: []string{"catalog.Currency"}}, []string{"RON", "EUR"}},
		{"constant by name", GetFactsArgs{Constants: []string{"httpapi.maxQty"}}, []string{"maxQty"}},
		{"functions", GetFactsArgs{Functions: []string{"catalog.ParseCurrency", "(*catalog.Store).Get"}}, []string{"ParseCurrency", "(*Store).Get"}},
		{"not found", GetFactsArgs{Route: "GET /nope", Types: []string{"Nope"}}, []string{"route GET /nope", "type Nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetFacts(f, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if n := names(got); !reflect.DeepEqual(n, tt.want) {
				t.Errorf("GetFacts() = %v, want %v", n, tt.want)
			}
		})
	}
	if _, err := GetFacts(f, GetFactsArgs{}); err == nil {
		t.Error("GetFacts with no question succeeded, want error")
	}
}
