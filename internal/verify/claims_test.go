package verify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
)

// claimsRepo is a small service whose docs cite its code in every way the
// splitter resolves.
var claimsRepo = map[string]string{
	"go.mod":   "module shop\n",
	"Makefile": "BIN ?= bin/shop\n\ntest:\n\tgo test ./...\n\nbuild:\n\tgo build -o $(BIN) ./cmd/shop\n",
	"internal/shop/price.go": `package shop

// rates are units of the currency per 1 unit of the base currency.
var rates = map[string]float64{"EUR": 0.2}

// Convert turns an amount in cents into the target currency.
func Convert(cents int64, to string) float64 {
	return float64(cents) / 100 * rates[to]
}

type Kind string

const (
	KindA Kind = "a"
	KindB Kind = "b"
)
`,
	"internal/shop/handlers.go": `package shop

import "net/http"

const maxQty = 10

type Store struct{}

// Get writes one order.
// @Summary  Get an order
// @Router   /orders/{id} [get]
func (s *Store) Get(w http.ResponseWriter, r *http.Request) {
	_ = maxQty
	_ = OrderResponse{}
}
`,
	"internal/shop/types.go": "package shop\n\ntype OrderResponse struct {\n\tTotal    float64 `json:\"total\"`\n\tDiscount int     `json:\"discount,omitempty\"`\n}\n",
}

const claimsPage = "# Business rules\n" + // 1
	"\n" +
	"Intro prose line without any citation here.\n" + // 3: uncited
	"\n" +
	"- `internal/shop/price.go:Convert` converts an amount in cents:\n" + // 5
	"  1. Multiply by the rate: `100 cents → 100 * 0.2 = 20 EUR`\n" + // 6: inherits 5
	"- The kinds are listed in `` `internal/shop/price.go:shop.KindA` `` and below it.\n" + // 7
	"\n" +
	"```go\n" +
	"// `internal/shop/price.go:Convert` in a fence is no claim\n" +
	"```\n" +
	"\n" +
	"## GET /orders/{id}\n" + // 13
	"\n" +
	"| Field | Meaning |\n" +
	"|---|---|\n" +
	"| discount | always present |\n" + // 17: inherits the route's handler
	"\n" +
	"Run the tests with `make test`.\n" + // 19
	"`internal/shop/price.go:Convert`\n" + // 20: only a reference
	"The handler at `internal/shop/handlers.go:12` checks the quantity, see `shop.(*Store).Get`.\n" + // 21
	"Amounts are converted from cents [internal/shop/price.go:Convert], see the [API](api.md).\n" + // 22: a bracketed citation
	"See the [Price](internal/shop/price.go) page and [notes] for the rest.\n" // 23: a link and brackets are no citation

func claimsFixture(t *testing.T) (string, *gofacts.Facts) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"docs/business-rules.md": claimsPage}
	for k, v := range claimsRepo {
		files[k] = v
	}
	for _, p := range DocPages {
		if _, ok := files[p]; !ok {
			files[p] = "# Page\n"
		}
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, &gofacts.Facts{
		Packages: []gofacts.Package{{Path: "shop/internal/shop", Name: "shop", Dir: "internal/shop",
			Files: []string{"internal/shop/handlers.go", "internal/shop/price.go", "internal/shop/types.go"}}},
		Routes: []gofacts.Route{{Pattern: "GET /orders/{id}", Method: "GET", Path: "/orders/{id}",
			Handler: "shop.(*Store).Get", HandlerPos: "internal/shop/handlers.go:12"}},
		MakeTargets: []gofacts.MakeTarget{{Name: "test"}, {Name: "build"}},
	}
}

func TestSplitClaims(t *testing.T) {
	dir, facts := claimsFixture(t)
	pages, err := SplitClaims(dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	pc := pages["docs/business-rules.md"]
	if want := []int{3}; !reflect.DeepEqual(pc.Uncited, want) {
		t.Errorf("uncited lines = %v, want %v", pc.Uncited, want)
	}
	byLine := map[int]Claim{}
	var lines []int
	for _, c := range pc.Claims {
		byLine[c.Line] = c
		lines = append(lines, c.Line)
	}
	if want := []int{5, 6, 7, 17, 19, 21, 22, 23}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("claim lines = %v, want %v", lines, want)
	}
	for _, p := range DocPages {
		if p != "docs/business-rules.md" && (len(pages[p].Claims) > 0 || len(pages[p].Uncited) > 0) {
			t.Errorf("%s: %+v, want nothing for a title-only page", p, pages[p])
		}
	}

	snippet := func(line int) string {
		var s []string
		for _, c := range byLine[line].Citations {
			s = append(s, c.Snippet)
		}
		return strings.Join(s, "\n")
	}
	for _, tt := range []struct {
		line      int
		inherited bool
		has       []string // in the snippet
		hasNot    []string
		context   []string
	}{
		// The cited function and the package-level names it uses.
		{line: 5, has: []string{"   8  \treturn float64(cents) / 100 * rates[to]", "var rates = map"}, hasNot: []string{"KindA"}},
		{line: 6, inherited: true, has: []string{"float64(cents) / 100"}, context: []string{"- `internal/shop/price.go:Convert` converts"}},
		// Double backticks and a package prefix; a const group comes whole.
		{line: 7, has: []string{"KindA Kind", "KindB Kind"}},
		// A route heading cites its handler; its types come from the
		// package's other files, and swag lines are not evidence.
		{line: 17, inherited: true, has: []string{"func (s *Store) Get", "const maxQty = 10", "// internal/shop/types.go", "`json:\"discount,omitempty\"`"},
			hasNot: []string{"@Summary", "@Router"}, context: []string{"## GET /orders/{id}", "| Field | Meaning |"}},
		{line: 19, has: []string{"   3  test:", "go test ./..."}, hasNot: []string{"go build"}},
		// file:line resolves to the declaration around it; pkg.(*T).M to the method.
		{line: 21, has: []string{"func (s *Store) Get"}},
		{line: 22, has: []string{"func Convert"}},
		// A link and plain brackets cite nothing, so the section's route is cited.
		{line: 23, inherited: true, has: []string{"func (s *Store) Get"}},
	} {
		c := byLine[tt.line]
		if c.Inherited != tt.inherited {
			t.Errorf("line %d: inherited = %v, want %v", tt.line, c.Inherited, tt.inherited)
		}
		s := snippet(tt.line)
		for _, want := range tt.has {
			if !strings.Contains(s, want) {
				t.Errorf("line %d: snippet lacks %q:\n%s", tt.line, want, s)
			}
		}
		for _, bad := range tt.hasNot {
			if strings.Contains(s, bad) {
				t.Errorf("line %d: snippet has %q:\n%s", tt.line, bad, s)
			}
		}
		for _, want := range tt.context {
			if !strings.Contains(c.Context, want) {
				t.Errorf("line %d: context %q lacks %q", tt.line, c.Context, want)
			}
		}
	}
	if n := len(byLine[21].Citations); n != 1 {
		t.Errorf("line 21: %d citations, want 1 (both refs name the same method)", n)
	}
}

func TestSplitClaimsSkipsUnresolved(t *testing.T) {
	dir, facts := claimsFixture(t)
	page := "# Rules\n\nThe total is computed in `internal/shop/price.go:Missing` by the store.\n" +
		"The order route `GET /nowhere` returns the order total.\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "business-rules.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := SplitClaims(dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	pc := pages["docs/business-rules.md"]
	// Broken references are gate B's to report; here they leave the lines uncited.
	if len(pc.Claims) != 0 || !reflect.DeepEqual(pc.Uncited, []int{3, 4}) {
		t.Errorf("claims %+v, uncited %v; want none and [3 4]", pc.Claims, pc.Uncited)
	}
}
