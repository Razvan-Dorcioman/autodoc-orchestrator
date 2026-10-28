package pipeline

import (
	"strings"
	"testing"

	"autodoc/internal/repo"
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

// TestSpecGapsReported: constraint issues gate A's last attempt let through
// reach the commit message and summary.json.
func TestSpecGapsReported(t *testing.T) {
	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	res := &Result{Repo: &repo.Repo{BaseSHA: "abc"}, Main: "main.go", SpecGaps: []verify.Issue{
		{Check: verify.ConstraintCheck, Where: "GET /items", Message: "query param qty: missing maximum(10)"},
	}}
	want := "GET /items: query param qty: missing maximum(10)"
	if msg := commitMessage(rec, res); !strings.Contains(msg, "still found 1 param constraint(s)") || !strings.Contains(msg, "- "+want) {
		t.Errorf("commit message lacks the constraint:\n%s", msg)
	}
	if o := res.outcome(); len(o.SpecGaps) != 1 || o.SpecGaps[0] != want {
		t.Errorf("outcome spec_constraints = %v", o.SpecGaps)
	}
}
