package verify

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"autodoc/internal/gofacts"
	"autodoc/internal/strip"
)

// spec is the part of a Swagger 2.0 document the gate compares with facts.
type spec struct {
	Swagger string `json:"swagger"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	BasePath    string                                `json:"basePath"`
	Paths       map[string]map[string]json.RawMessage `json:"paths"`
	Definitions map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"definitions"`
}

type operation struct {
	Parameters []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"parameters"`
	Responses map[string]struct {
		Description string `json:"description"`
		Schema      *struct {
			Ref string `json:"$ref"`
		} `json:"schema"`
	} `json:"responses"`
}

var httpMethods = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true}

const defsPrefix = "#/definitions/"

// validateSpec checks that swagger.json is a well-formed Swagger 2.0
// document whose references resolve, and that swagger.yaml says the same.
func validateSpec(jsonData, yamlData []byte) []Issue {
	issue := func(format string, args ...any) Issue {
		return Issue{Check: "swag", Where: strip.SpecDir + "/swagger.json", Message: fmt.Sprintf(format, args...)}
	}
	var s spec
	if err := json.Unmarshal(jsonData, &s); err != nil {
		return []Issue{issue("not valid JSON: %v", err)}
	}
	var out []Issue
	if s.Swagger != "2.0" {
		out = append(out, issue("swagger is %q, want \"2.0\"", s.Swagger))
	}
	if s.Info.Title == "" || s.Info.Version == "" {
		out = append(out, issue("info.title and info.version are required: add @title and @version to the general API info on main"))
	}
	if len(s.Paths) == 0 {
		out = append(out, issue("no paths: no handler has a @Router annotation"))
	}
	var doc any
	_ = json.Unmarshal(jsonData, &doc)
	for _, ref := range refs(doc) {
		name, ok := strings.CutPrefix(ref, defsPrefix)
		if _, defined := s.Definitions[name]; !ok || !defined {
			out = append(out, issue("$ref %q does not resolve to a definition", ref))
		}
	}

	var y any
	if err := yaml.Unmarshal(yamlData, &y); err != nil {
		return append(out, Issue{Check: "swag", Where: strip.SpecDir + "/swagger.yaml", Message: "not valid YAML: " + err.Error()})
	}
	// Compare through JSON so YAML's int and JSON's float64 numbers agree.
	yj, err := json.Marshal(y)
	var fromYAML any
	if err == nil {
		err = json.Unmarshal(yj, &fromYAML)
	}
	if err != nil || !reflect.DeepEqual(doc, fromYAML) {
		out = append(out, Issue{Check: "swag", Where: strip.SpecDir + "/swagger.yaml", Message: "does not describe the same document as swagger.json"})
	}
	return out
}

// refs collects every "$ref" string in a decoded JSON document.
func refs(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == "$ref" {
				out = append(out, s)
			} else {
				out = append(out, refs(e)...)
			}
		}
	case []any:
		for _, e := range x {
			out = append(out, refs(e)...)
		}
	}
	return out
}

// checkSpec compares the spec with facts.json: the same operations, the same
// path and query params, every status code each handler writes and no other,
// the body type of each response, every error code named in its response
// description, and the JSON field names of every documented type.
func checkSpec(jsonData []byte, f *gofacts.Facts) []Issue {
	var s spec
	if err := json.Unmarshal(jsonData, &s); err != nil {
		return []Issue{{Check: "spec", Message: "swagger.json does not parse: " + err.Error()}}
	}
	var out []Issue
	add := func(where, format string, args ...any) {
		out = append(out, Issue{Check: "spec", Where: where, Message: fmt.Sprintf(format, args...)})
	}

	ops := map[string]operation{} // "get /products/{id}" as served, basePath included
	for p, item := range s.Paths {
		for m, raw := range item {
			if !httpMethods[m] {
				continue
			}
			var op operation
			if err := json.Unmarshal(raw, &op); err != nil {
				add(m+" "+p, "operation does not parse: %v", err)
				continue
			}
			ops[m+" "+joinPath(s.BasePath, p)] = op
		}
	}

	seen := map[string]bool{}
	for _, r := range f.Routes {
		key := strings.ToLower(r.Method) + " " + r.Path
		where := fmt.Sprintf("%s (handler %s at %s)", r.Pattern, r.Handler, r.HandlerPos)
		op, ok := ops[key]
		if !ok {
			add(where, "route is not in the spec: annotate the handler with @Router %s [%s]", r.Path, strings.ToLower(r.Method))
			continue
		}
		seen[key] = true

		want := map[string]bool{}
		for _, p := range r.Params {
			want[p.In+" "+p.Name] = true
		}
		got := map[string]bool{}
		for _, p := range op.Parameters {
			k := p.In + " " + p.Name
			got[k] = true
			switch {
			case !want[k]:
				add(where, "@Param %s (in %s) is not read by the handler; remove it", p.Name, p.In)
			case p.In == "path" && !p.Required:
				add(where, "path param %s must be required (true)", p.Name)
			}
		}
		for _, p := range r.Params {
			if !got[p.In+" "+p.Name] {
				add(where, "missing @Param %s %s: the handler reads it at %s", p.Name, p.In, p.Pos)
			}
		}

		byStatus := map[string][]gofacts.ResponseSite{}
		for _, site := range r.Responses {
			k := strconv.Itoa(site.Status)
			byStatus[k] = append(byStatus[k], site)
		}
		for code := range op.Responses {
			if _, ok := byStatus[code]; !ok {
				add(where, "response %s is documented but the handler never writes it; remove it", code)
			}
		}
		for _, code := range sortedKeys(byStatus) {
			sites := byStatus[code]
			resp, ok := op.Responses[code]
			if !ok {
				add(where, "status %s is not documented: the handler writes it at %s", code, positions(sites))
				continue
			}
			if body := sameBody(sites); body != "" {
				switch {
				case resp.Schema == nil:
					add(where, "response %s has no schema; the handler writes {object} %s", code, body)
				case resp.Schema.Ref != defsPrefix+body:
					add(where, "response %s schema is %s; the handler writes {object} %s", code, strings.TrimPrefix(resp.Schema.Ref, defsPrefix), body)
				}
			}
			for _, site := range sites {
				if site.Code != "" && !strings.Contains(resp.Description, site.Code) {
					add(where, "response %s description must name error code %q (written at %s)", code, site.Code, site.Pos)
				}
			}
		}
	}
	for _, key := range sortedKeys(ops) {
		if !seen[key] {
			add(key, "operation is not a route the service registers; remove its @Router")
		}
	}

	types := map[string]gofacts.Type{}
	for _, t := range f.Types {
		types[t.Package+"."+t.Name] = t
	}
	for _, name := range sortedKeys(s.Definitions) {
		t, ok := types[name]
		if !ok || t.Underlying != "struct" {
			continue
		}
		want, ok := jsonFields(t)
		if !ok {
			continue
		}
		got := s.Definitions[name].Properties
		for _, fld := range sortedKeys(want) {
			if _, ok := got[fld]; !ok {
				add("definition "+name, "missing property %q from the JSON tags of %s (%s)", fld, name, t.Pos)
			}
		}
		for _, fld := range sortedKeys(got) {
			if !want[fld] {
				add("definition "+name, "property %q is not a JSON field of %s (%s)", fld, name, t.Pos)
			}
		}
	}
	return out
}

// jsonFields lists the JSON names encoding/json uses for t; ok is false for
// types with embedded fields, whose promoted fields facts do not list.
func jsonFields(t gofacts.Type) (map[string]bool, bool) {
	out := map[string]bool{}
	for _, fl := range t.Fields {
		if fl.Embedded {
			return nil, false
		}
		if fl.JSON == "-" || !unicode.IsUpper([]rune(fl.Name)[0]) {
			continue
		}
		name := fl.JSON
		if name == "" {
			name = fl.Name
		}
		out[name] = true
	}
	return out, true
}

// sameBody is the body type all sites write, or "" if they differ.
func sameBody(sites []gofacts.ResponseSite) string {
	body := sites[0].Body
	for _, s := range sites[1:] {
		if s.Body != body {
			return ""
		}
	}
	return body
}

func positions(sites []gofacts.ResponseSite) string {
	var ps []string
	for _, s := range sites {
		p := s.Pos
		if s.Code != "" {
			p += " (" + s.Code + ")"
		}
		ps = append(ps, p)
	}
	return strings.Join(ps, ", ")
}

// joinPath prefixes an operation path with the spec's basePath.
func joinPath(base, p string) string {
	if base == "" || base == "/" {
		return p
	}
	return path.Join(base, p)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
