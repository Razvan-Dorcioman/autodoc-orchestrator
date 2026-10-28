package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
)

// TestGateAGreenOnTestSvc annotates the gofacts test service by hand the way
// the swagger agent is told to, runs the pinned swag on it and expects gate A
// green: a method switch split into one operation per method, a handler that
// checks r.Method itself, request bodies, a slice response.
func TestGateAGreenOnTestSvc(t *testing.T) {
	if testing.Short() {
		t.Skip("runs swag")
	}
	ctx := context.Background()
	src := "../gofacts/testdata/svc"
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	base, err := guard.Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := gofacts.Extract(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	annotate := func(rel, funcLine string, lines ...string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		block := "// @" + strings.Join(lines, "\n// @") + "\n"
		edited := strings.Replace(string(data), "\n"+funcLine, "\n"+block+funcLine, 1)
		if edited == string(data) {
			t.Fatalf("%s: no %q", rel, funcLine)
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const api, orders = "internal/api/api.go", "internal/api/orders.go"
	annotate("cmd/server/main.go", "func main()", "title Test service", "version 1.0", "BasePath /")
	annotate(api, "func index(",
		"Summary List items", "Produce json",
		"Param limit query int false \"Maximum items\" maximum(20) default(5)",
		"Success 200 {object} api.ItemsResponse",
		"Failure 400 {object} api.ErrorResponse \"invalid_limit\"",
		"Router / [get]")
	annotate(api, "func item(",
		"Summary Get an item", "Produce json",
		"Param id path int true \"Item ID\"",
		"Param color query string false \"Color\" Enums(red,blue)",
		"Success 200 {object} api.Item",
		"Failure 400 {object} api.ErrorResponse \"invalid_id or invalid_color\"",
		"Failure 404 {object} api.ErrorResponse \"not_found\"",
		"Router /items/{id} [get]")
	annotate(orders, "func listOrders(",
		"Summary List orders", "Produce json",
		"Success 200 {array} api.Order",
		"Router /orders [get]")
	annotate(orders, "func createOrder(",
		"Summary Place an order", "Accept json", "Produce json",
		"Param body body api.CreateOrder true \"The order\"",
		"Success 201 {object} api.Order",
		"Failure 400 {object} api.ErrorResponse \"invalid_body\"",
		"Failure 422 {object} api.ErrorResponse \"invalid_qty\"",
		"Router /orders [post]")
	annotate(orders, "func notes(",
		"Summary Replace the note", "Accept json", "Produce json",
		"Param body body api.Note true \"The note\"",
		"Success 200 {object} api.Note",
		"Failure 400 {object} api.ErrorResponse \"invalid_body\"",
		"Failure 405 {object} api.ErrorResponse \"method_not_allowed\"",
		"Router /notes [put]")

	rep := GateA(ctx, GateAInput{
		Dir:        dir,
		Main:       "cmd/server/main.go",
		Baseline:   base,
		BaseChecks: []CheckResult{{Name: "build", OK: true}, {Name: "vet", OK: true}},
		Original:   func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(src, filepath.FromSlash(rel))) },
		Facts:      facts,
	})
	// The fixture's POST /files/{path...} is an http.NotFoundHandler(): no
	// function to annotate, so it is the one expected issue.
	var other []string
	for _, is := range rep.Issues {
		if !strings.Contains(is.Where, "POST /files/{path...}") {
			other = append(other, is.Where+": "+is.Message)
		}
	}
	if len(other) > 0 || len(rep.Issues) != 1 {
		t.Errorf("gate A:\n%s", rep)
	}
}
