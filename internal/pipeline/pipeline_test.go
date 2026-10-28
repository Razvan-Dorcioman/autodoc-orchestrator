package pipeline

import (
	"strings"
	"testing"

	"autodoc/internal/gofacts"
	"autodoc/internal/verify"
)

func TestIssuesByPage(t *testing.T) {
	issues := []verify.Issue{
		{Check: "coverage", Where: "docs/api.md", Message: "missing a field"},
		{Check: "judge", Where: "docs/business-rules.md:31", Message: "divides by 100 first"},
		{Check: "reference", Where: "docs/api.md:12", Message: "no route"},
	}
	got, err := issuesByPage(issues)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got["docs/api.md"]) != 2 || got["docs/business-rules.md"][0].Check != "judge" {
		t.Errorf("issuesByPage = %+v", got)
	}
	if _, err := issuesByPage(append(issues, verify.Issue{Check: "scope", Where: "internal/x.go"})); err == nil || !strings.Contains(err.Error(), "internal/x.go") {
		t.Errorf("an issue outside the pages: err = %v, want one naming it", err)
	}
}

func TestEntrypoint(t *testing.T) {
	f := &gofacts.Facts{Mains: []gofacts.Main{
		{File: "cmd/api/main.go", ServesRoutes: true},
		{File: "cmd/worker/main.go"},
	}}
	for _, tt := range []struct {
		override, want, err string
	}{
		{override: "", want: "cmd/api/main.go"},
		{override: "cmd/worker/main.go", want: "cmd/worker/main.go"},
		{override: "./cmd/worker/main.go", want: "cmd/worker/main.go"},
		{override: "main.go", err: "cmd/api/main.go, cmd/worker/main.go"},
	} {
		got, err := entrypoint(f, tt.override)
		if got != tt.want || (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("entrypoint(%q) = %q, %v; want %q, error containing %q", tt.override, got, err, tt.want, tt.err)
		}
	}
}
