package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/specroute"
	"autodoc/internal/strip"
)

// TestRoute applies the template to a copy of the gofacts test service and
// runs the add-route check on it: green as applied, red after a code edit.
func TestRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("builds, vets and type-checks a module")
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
	plan, err := specroute.PlanFor(dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := specroute.Apply(dir, plan); err != nil {
		t.Fatal(err)
	}
	// Gate A has written the spec by the time the route is added.
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(strip.SpecDir), "swagger.json"), []byte(`{"swagger":"2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in := RouteInput{
		Dir:        dir,
		Plan:       plan,
		Baseline:   base,
		BaseChecks: []CheckResult{{Name: "build", OK: true}, {Name: "vet", OK: true}},
		Original:   func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(src, filepath.FromSlash(rel))) },
	}
	if rep := Route(ctx, in); !rep.OK() {
		t.Fatalf("after Apply: %s", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(selfTestFile))); !os.IsNotExist(err) {
		t.Errorf("the self-test was left behind: %v", err)
	}
	if err := specroute.Apply(dir, plan); err != nil {
		t.Fatal(err)
	}
	if rep := Route(ctx, in); !rep.OK() {
		t.Fatalf("after a second Apply: %s", rep)
	}

	file := filepath.Join(dir, filepath.FromSlash(plan.File))
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `mux.Handle("GET /openapi.json"`, `mux.Handle("GET /spec.json"`, 1)
	if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := Route(ctx, in)
	var got []string
	for _, is := range rep.Issues {
		got = append(got, is.Check+" "+is.Where)
	}
	if strings.Join(got, "; ") != "route internal/api/api.go; code internal/api/api.go" {
		t.Errorf("after editing the route line: issues = %q\n%s", got, rep)
	}
}

// TestGateAWithoutSpec is a re-run where swag fails: the apispec.go of an
// earlier run embeds the swagger.json strip deleted, so gate A reports swag
// and leaves the build, which could only fail for that reason, unchecked.
func TestGateAWithoutSpec(t *testing.T) {
	if testing.Short() {
		t.Skip("runs swag")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../gofacts/testdata/svc")); err != nil {
		t.Fatal(err)
	}
	facts, err := gofacts.Extract(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := specroute.PlanFor(dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := specroute.Apply(dir, plan); err != nil {
		t.Fatal(err)
	}
	base, err := guard.Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	rep := GateA(ctx, GateAInput{
		Dir:        dir,
		Main:       "cmd/server/missing.go", // swag fails
		Baseline:   base,
		BaseChecks: []CheckResult{{Name: "build", OK: true}, {Name: "vet", OK: true}},
		Original:   func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) },
		Facts:      facts,
	})
	var checks, issues []string
	for _, c := range rep.Checks {
		checks = append(checks, c.Name)
	}
	for _, is := range rep.Issues {
		issues = append(issues, is.Check)
	}
	if strings.Join(checks, " ") != "swag" || strings.Join(issues, " ") != "swag" {
		t.Errorf("checks %q, issues %q; want only swag:\n%s", checks, issues, rep)
	}
}
