package gofacts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// constraintSvc covers each pattern paramConstraints reads, and two it must
// not: a bound that answers 404 and a check on a variable the param never
// reached.
const constraintSvc = `package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type Size string

const (
	Small Size = "s"
	Large Size = "l"
)

func parseSize(s string) (Size, error) {
	switch Size(strings.ToLower(s)) {
	case Small, Large:
		return Size(s), nil
	}
	return "", errors.New("bad size")
}

const maxPage = 50

func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", list)
	return mux
}

func list(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || 0 >= id {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if id > 1000 {
		http.Error(w, "no such item", http.StatusNotFound)
		return
	}
	q := r.URL.Query()

	page := 1
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n >= maxPage+1 {
			http.Error(w, "bad page", http.StatusUnprocessableEntity)
			return
		}
		page = n
	}

	size := Small
	if raw := q.Get("size"); raw != "" {
		if size, err = parseSize(raw); err != nil {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
	}

	sort := q.Get("sort")
	switch sort {
	case "name", "price":
	default:
		http.Error(w, "bad sort", http.StatusBadRequest)
		return
	}

	order := q.Get("order")
	if order != "asc" && order != "desc" {
		http.Error(w, "bad order", http.StatusBadRequest)
		return
	}

	ratio, _ := strconv.ParseFloat(q.Get("ratio"), 64)
	if ratio < 0.5 || ratio <= 0.1 {
		http.Error(w, "bad ratio", http.StatusBadRequest)
		return
	}

	other := 3
	if other > 2 {
		http.Error(w, "never", http.StatusBadRequest)
		return
	}
	_, _, _ = page, size, other
}
`

func TestParamConstraints(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/c\n\ngo 1.22\n")
	write("api/api.go", constraintSvc)
	f, err := Extract(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 1 {
		t.Fatalf("routes = %+v", f.Routes)
	}
	got := map[string]Param{}
	for _, p := range f.Routes[0].Params {
		got[p.In+" "+p.Name] = p
	}
	at := func(line string) string { return "api/api.go:" + line }
	c := func(v, line string) *Constraint { return &Constraint{Value: v, Pos: at(line)} }
	want := map[string]Param{
		// 0 >= id on an int: flipped, then moved by one. The 404 bound is not
		// a validation and is left out.
		"path id": {Minimum: c("1", "35")},
		// The default is on the variable the parsed value lands in; >= on an
		// int moves the maximum down by one.
		"query page": {Default: c("1", "45"), Maximum: c("50", "48")},
		"query size": {Default: c("s", "55"), Enum: &EnumConstraint{
			Values: []string{"s", "l"}, Pos: at("57"), Via: "api.parseSize returns a api.Size; these are its constants",
		}},
		"query sort":  {Enum: &EnumConstraint{Values: []string{"name", "price"}, Pos: at("64"), Via: "the handler's switch rejects any other value"}},
		"query order": {Enum: &EnumConstraint{Values: []string{"asc", "desc"}, Pos: at("72"), Via: "the handler rejects any other value"}},
		// A strict bound on a float is exclusive and not recorded.
		"query ratio": {Minimum: c("0.5", "78")},
	}
	if len(got) != len(want) {
		t.Errorf("params = %v, want %d", keysOf(got), len(want))
	}
	for k, w := range want {
		g := got[k]
		g.Name, g.In, g.Pos = "", "", ""
		if gj, wj := asJSON(g), asJSON(w); gj != wj {
			t.Errorf("%s:\n got %s\nwant %s", k, gj, wj)
		}
	}
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func keysOf(m map[string]Param) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
