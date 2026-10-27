package tools

import (
	"errors"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"autodoc/internal/gofacts"
)

type GetFactsArgs struct {
	Route     string   `json:"route,omitempty" jsonschema:"one route by pattern (GET /products/{id}), path (/products/{id}) or handler (httpapi.(*handlers).product); '*' lists all routes in brief"`
	Types     []string `json:"types,omitempty" jsonschema:"type names, bare (ProductResponse) or package-qualified (httpapi.ProductResponse)"`
	Constants []string `json:"constants,omitempty" jsonschema:"constant names, bare or package-qualified, or a type name (catalog.Currency) for all constants of that type; '*' for all"`
	Functions []string `json:"functions,omitempty" jsonschema:"function names, e.g. catalog.ParseCurrency or (*catalog.Store).Get: signature, position and doc"`
}

// RouteBrief is one line of the route list.
type RouteBrief struct {
	Pattern    string `json:"pattern"`
	Handler    string `json:"handler"`
	HandlerPos string `json:"handler_pos,omitempty"`
}

type GetFactsResult struct {
	Routes    []RouteBrief       `json:"routes,omitempty"`
	Route     *gofacts.Route     `json:"route,omitempty"`
	Types     []gofacts.Type     `json:"types,omitempty"`
	Constants []gofacts.Constant `json:"constants,omitempty"`
	Functions []gofacts.Function `json:"functions,omitempty"`
	NotFound  []string           `json:"not_found,omitempty"`
}

// NewGetFacts returns the get_facts tool over the facts extracted by the index step.
func NewGetFacts(f *gofacts.Facts) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: "get_facts",
		Description: "Look up facts extracted from the type-checked source (facts.json): a route with its params and every " +
			"response it writes, types with their JSON field names, constants and functions. Ask only for what you need.",
	}, func(_ agent.Context, args GetFactsArgs) (GetFactsResult, error) {
		return GetFacts(f, args)
	})
}

// GetFacts is get_facts without the ADK wrapper, so it can be tested directly.
func GetFacts(f *gofacts.Facts, args GetFactsArgs) (GetFactsResult, error) {
	if args.Route == "" && len(args.Types) == 0 && len(args.Constants) == 0 && len(args.Functions) == 0 {
		return GetFactsResult{}, errors.New("ask for at least one of route, types, constants or functions")
	}
	var res GetFactsResult
	switch r := strings.TrimSpace(args.Route); r {
	case "":
	case "*":
		for _, rt := range f.Routes {
			res.Routes = append(res.Routes, RouteBrief{Pattern: rt.Pattern, Handler: rt.Handler, HandlerPos: rt.HandlerPos})
		}
	default:
		for i, rt := range f.Routes {
			if r == rt.Pattern || r == rt.Path || r == rt.Handler || strings.HasSuffix(rt.Handler, "."+r) {
				res.Route = &f.Routes[i]
				break
			}
		}
		if res.Route == nil {
			res.NotFound = append(res.NotFound, "route "+r)
		}
	}
	for _, name := range args.Types {
		found := false
		for _, t := range f.Types {
			if matches(name, t.Package, t.Name) {
				res.Types, found = append(res.Types, t), true
			}
		}
		if !found {
			res.NotFound = append(res.NotFound, "type "+name)
		}
	}
	for _, name := range args.Constants {
		found := false
		for _, c := range f.Constants {
			if name == "*" || matches(name, c.Package, c.Name) || name == c.Type {
				res.Constants, found = append(res.Constants, c), true
			}
		}
		if !found {
			res.NotFound = append(res.NotFound, "constant "+name)
		}
	}
	for _, name := range args.Functions {
		found := false
		for _, fn := range f.Functions {
			if matches(name, fn.Package, fn.Name) || name == qualifiedMethod(fn) {
				res.Functions, found = append(res.Functions, fn), true
			}
		}
		if !found {
			res.NotFound = append(res.NotFound, "function "+name)
		}
	}
	return res, nil
}

// matches accepts "Name" and "pkg.Name".
func matches(query, pkg, name string) bool {
	return query == name || query == pkg+"."+name
}

// qualifiedMethod writes "(*Store).Get" in package catalog as "(*catalog.Store).Get".
func qualifiedMethod(fn gofacts.Function) string {
	if !strings.HasPrefix(fn.Name, "(") {
		return ""
	}
	recv, method, _ := strings.Cut(fn.Name[1:], ").")
	star := ""
	if strings.HasPrefix(recv, "*") {
		star, recv = "*", recv[1:]
	}
	return "(" + star + fn.Package + "." + recv + ")." + method
}
