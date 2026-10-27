package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"autodoc/internal/strip"
)

type GetSpecArgs struct {
	Path   string `json:"path,omitempty" jsonschema:"operation path as in the spec, e.g. /products/{id}; empty for every operation"`
	Method string `json:"method,omitempty" jsonschema:"HTTP method, e.g. get; empty for every method of the path"`
}

// SpecOperation is one operation of the spec as swag wrote it.
type SpecOperation struct {
	Method    string         `json:"method"`
	Path      string         `json:"path"`
	Operation map[string]any `json:"operation"`
}

type GetSpecResult struct {
	Info        map[string]any  `json:"info,omitempty"`
	BasePath    string          `json:"base_path,omitempty"`
	Operations  []SpecOperation `json:"operations"`
	Definitions map[string]any  `json:"definitions,omitempty"` // the ones the operations reference, transitively
}

// NewGetSpec returns the get_spec tool over the verified swagger.json in
// root. The spec is read once: it is final by the time the docs agent runs.
func NewGetSpec(root string) (tool.Tool, error) {
	doc, err := LoadSpec(root)
	if err != nil {
		return nil, err
	}
	return functiontool.New(functiontool.Config{
		Name: "get_spec",
		Description: "Look up the verified OpenAPI (Swagger 2.0) spec of the service: operations with their parameters, " +
			"responses and error codes, and the definitions they reference. Filter by path and method.",
	}, func(_ agent.Context, args GetSpecArgs) (GetSpecResult, error) {
		return GetSpec(doc, args)
	})
}

// LoadSpec reads the generated swagger.json from the spec package in root.
func LoadSpec(root string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(strip.SpecDir), "swagger.json"))
	if err != nil {
		return nil, fmt.Errorf("read the spec: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse the spec: %w", err)
	}
	return doc, nil
}

// GetSpec is get_spec without the ADK wrapper, so it can be tested directly.
func GetSpec(doc map[string]any, args GetSpecArgs) (GetSpecResult, error) {
	res := GetSpecResult{Operations: []SpecOperation{}}
	res.Info, _ = doc["info"].(map[string]any)
	res.BasePath, _ = doc["basePath"].(string)
	paths, _ := doc["paths"].(map[string]any)
	want := strings.TrimSpace(args.Path)
	if want != "" && paths[want] == nil && res.BasePath != "" && res.BasePath != "/" {
		want = strings.TrimPrefix(want, strings.TrimRight(res.BasePath, "/"))
	}
	method := strings.ToLower(strings.TrimSpace(args.Method))
	for _, p := range sortedKeys(paths) {
		if want != "" && p != want {
			continue
		}
		item, _ := paths[p].(map[string]any)
		for _, m := range sortedKeys(item) {
			op, ok := item[m].(map[string]any)
			if !ok || method != "" && m != method {
				continue
			}
			res.Operations = append(res.Operations, SpecOperation{Method: m, Path: p, Operation: op})
		}
	}
	if len(res.Operations) == 0 {
		return GetSpecResult{}, fmt.Errorf("no operation %s %s in the spec; its paths are %s", args.Method, args.Path, strings.Join(sortedKeys(paths), ", "))
	}

	defs, _ := doc["definitions"].(map[string]any)
	res.Definitions = map[string]any{}
	var visit func(v any)
	visit = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				name := strings.TrimPrefix(ref, "#/definitions/")
				if d, ok := defs[name]; ok && res.Definitions[name] == nil {
					res.Definitions[name] = d
					visit(d)
				}
			}
			for _, k := range sortedKeys(x) {
				visit(x[k])
			}
		case []any:
			for _, e := range x {
				visit(e)
			}
		}
	}
	for _, op := range res.Operations {
		visit(op.Operation)
	}

	if data, err := json.Marshal(res); err == nil && len(data) > maxReadBytes {
		return GetSpecResult{}, fmt.Errorf("the result is %d bytes, more than the %d a call may return: ask for one path", len(data), maxReadBytes)
	}
	return res, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
