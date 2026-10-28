package verify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"autodoc/internal/gofacts"
)

// ConstraintCheck is the Check of the issues checkConstraints finds. They
// block gate A until its last attempt, then become advisory: a missing
// default or bound leaves the spec incomplete, not wrong about the routes,
// and the facts read constraints only from patterns, so a miss is possible.
const ConstraintCheck = "constraint"

// Blocking is the issues that fail the gate: all but ConstraintCheck ones.
func (r Report) Blocking() []Issue {
	return slices.DeleteFunc(slices.Clone(r.Issues), func(is Issue) bool { return is.Check == ConstraintCheck })
}

// specParam is a parameter of an operation with the constraints Swagger 2.0
// puts on it: swag's default(), Enums(), minimum() and maximum().
type specParam struct {
	Name    string `json:"name"`
	In      string `json:"in"`
	Default any    `json:"default"`
	Enum    []any  `json:"enum"`
	Minimum any    `json:"minimum"`
	Maximum any    `json:"maximum"`
}

// checkConstraints compares the default, the allowed values and the bounds
// of each path and query param in the spec with what the handler's code
// enforces, as facts.json records it. A constraint the code shows must be
// in the spec with the same value; one the facts do not know is left alone.
// Routes or params missing from the spec are checkSpec's to report.
func checkConstraints(jsonData []byte, f *gofacts.Facts) []Issue {
	var s spec
	if err := json.Unmarshal(jsonData, &s); err != nil {
		return nil // checkSpec reports it
	}
	params := map[string][]specParam{} // "get /products/{id}", basePath included
	for p, item := range s.Paths {
		for m, raw := range item {
			if !httpMethods[m] {
				continue
			}
			var op struct {
				Parameters []specParam `json:"parameters"`
			}
			if json.Unmarshal(raw, &op) == nil {
				params[m+" "+joinPath(s.BasePath, p)] = op.Parameters
			}
		}
	}

	var out []Issue
	for _, r := range f.Routes {
		methods := r.OpMethods()
		if len(methods) == 0 { // every method the spec has on the path
			for _, key := range sortedKeys(params) {
				if m, p, _ := strings.Cut(key, " "); p == r.Path {
					methods = append(methods, strings.ToUpper(m))
				}
			}
		}
		for _, m := range methods {
			ps, ok := params[strings.ToLower(m)+" "+r.Path]
			if !ok {
				continue
			}
			where := fmt.Sprintf("%s (handler %s at %s)", r.Pattern, r.Handler, r.HandlerPos)
			if len(methods) > 1 {
				where += " " + m
			}
			for _, want := range r.Params {
				i := slices.IndexFunc(ps, func(p specParam) bool { return p.Name == want.Name && p.In == want.In })
				if i < 0 {
					continue
				}
				for _, msg := range compareParam(want, ps[i]) {
					out = append(out, Issue{Check: ConstraintCheck, Where: where, Message: msg})
				}
			}
		}
	}
	return out
}

// compareParam says how the spec's param got differs from what the code
// enforces, in terms the swagger agent can act on: the @Param attribute to
// add or correct.
func compareParam(want gofacts.Param, got specParam) []string {
	var out []string
	name := fmt.Sprintf("%s param %s", want.In, want.Name)
	single := func(c *gofacts.Constraint, have any, attr, what string) {
		switch {
		case c == nil:
		case have == nil:
			out = append(out, fmt.Sprintf("%s: missing %s(%s): %s (%s); add it to its @Param line", name, attr, c.Value, what, c.Pos))
		case !sameValue(have, c.Value):
			out = append(out, fmt.Sprintf("%s: %s is %s in the spec, but %s (%s); set %s(%s) on its @Param line", name, attr, specText(have), what, c.Pos, attr, c.Value))
		}
	}
	if c := want.Default; c != nil {
		single(c, got.Default, "default", "the handler uses "+c.Value+" when it is absent")
	}
	if c := want.Minimum; c != nil {
		single(c, got.Minimum, "minimum", "the handler rejects values below "+c.Value)
	}
	if c := want.Maximum; c != nil {
		single(c, got.Maximum, "maximum", "the handler rejects values above "+c.Value)
	}
	if e := want.Enum; e != nil {
		attr := "Enums(" + strings.Join(e.Values, ",") + ")"
		why := "the handler accepts only " + strings.Join(e.Values, ", ")
		if e.Via != "" {
			why += ": " + e.Via
		}
		switch {
		case len(got.Enum) == 0:
			out = append(out, fmt.Sprintf("%s: missing %s: %s (%s); add it to its @Param line", name, attr, why, e.Pos))
		case !sameSet(got.Enum, e.Values):
			var have []string
			for _, v := range got.Enum {
				have = append(have, specText(v))
			}
			out = append(out, fmt.Sprintf("%s: the spec allows %s, but %s (%s); set %s on its @Param line", name, strings.Join(have, ", "), why, e.Pos, attr))
		}
	}
	return out
}

// sameValue compares a JSON value from the spec with a value from the
// facts: as numbers when both are numbers, else as text.
func sameValue(have any, want string) bool {
	if n, ok := have.(float64); ok {
		w, err := strconv.ParseFloat(want, 64)
		return err == nil && n == w
	}
	return specText(have) == want
}

func sameSet(have []any, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, h := range have {
		if !slices.ContainsFunc(want, func(w string) bool { return sameValue(h, w) }) {
			return false
		}
	}
	return true
}

func specText(v any) string {
	if n, ok := v.(float64); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
