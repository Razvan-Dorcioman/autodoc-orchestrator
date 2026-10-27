package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/specroute"
	"autodoc/internal/strip"
)

// RouteInput is what the add-route check compares the working copy with.
type RouteInput struct {
	Dir        string
	Plan       specroute.Plan
	Baseline   guard.Snapshot                   // from baseline.json
	BaseChecks []CheckResult                    // from baseline.json
	RunTests   bool                             // also run the target's tests, as an advisory check
	Original   func(rel string) ([]byte, error) // a file's content at the base commit
}

// selfTest is written into the spec package for one go test run and removed
// again: it proves the embedded document is the generated swagger.json and is
// served as JSON, without adding a test to the target.
const selfTest = `package apispec

import (
	"bytes"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAutodocServesSpec(t *testing.T) {
	want, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/openapi.json", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("GET /openapi.json = %d %q, %d bytes; want 200 application/json with swagger.json (%d bytes)",
			rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len(), len(want))
	}
}
`

const selfTestFile = strip.SpecDir + "/autodoc_selftest_test.go"

// Route checks the working copy after the add-route step: the spec package
// serves the spec, the route is registered once where the plan says on a
// net/http mux, the only code change is the template's edit of the plan's
// file, and no check that was green at the baseline is now red. Every check
// runs, so one report lists every problem.
func Route(ctx context.Context, in RouteInput) Report {
	rep := Report{Gate: "route", Issues: []Issue{}}
	add := func(is ...Issue) { rep.Issues = append(rep.Issues, is...) }

	t := serveCheck(ctx, in.Dir)
	rep.Checks = append(rep.Checks, t)
	if !t.OK {
		add(Issue{Check: "route", Where: specroute.File, Message: "the spec package does not serve swagger.json:\n" + t.Output})
	}

	if f, err := gofacts.Extract(ctx, in.Dir); err != nil {
		add(Issue{Check: "route", Message: "extract the routes: " + err.Error()})
	} else {
		var at []string
		for _, r := range f.Routes {
			if specroute.Own(r) {
				at = append(at, r.Registered)
			}
		}
		if len(at) != 1 || !strings.HasPrefix(at[0], in.Plan.File+":") {
			add(Issue{Check: "route", Where: in.Plan.File, Message: fmt.Sprintf("want %s registered once on a net/http mux in %s; found it at %v", specroute.Pattern, in.Plan.File, at)})
		}
	}

	// The baseline as it must read once the template has run: the planned
	// file with the route and import added, everything else as it was.
	orig, err := in.Original(in.Plan.File)
	var edited []byte
	if err == nil {
		edited, err = specroute.Insert(orig, in.Plan)
	}
	var fp guard.FileState
	if err == nil {
		fp, err = guard.Fingerprint(in.Plan.File, edited)
	}
	if err != nil {
		add(Issue{Check: "route", Where: in.Plan.File, Message: "apply the template to the original: " + err.Error()})
	} else {
		base := guard.Snapshot{Files: map[string]guard.FileState{}}
		for rel, st := range in.Baseline.Files {
			base.Files[rel] = st
		}
		base.Files[in.Plan.File] = fp
		original := func(rel string) ([]byte, error) {
			if rel == in.Plan.File {
				return edited, nil
			}
			return in.Original(rel)
		}
		cur, err := guard.Take(in.Dir)
		if err != nil {
			add(Issue{Check: "code", Message: "snapshot the working copy: " + err.Error()})
		} else {
			add(checkCode(base, cur)...)
			add(checkScope(base, cur, in.Dir, original)...)
		}
	}

	checks := Checks(ctx, in.Dir, in.RunTests)
	rep.Checks = append(rep.Checks, checks...)
	for _, c := range Regressions(in.BaseChecks, checks) {
		add(Issue{Check: "go", Where: c.Cmd, Message: c.Output})
	}
	return rep
}

// serveCheck runs selfTest in the spec package and removes it again.
func serveCheck(ctx context.Context, dir string) CheckResult {
	path := filepath.Join(dir, filepath.FromSlash(selfTestFile))
	if err := os.WriteFile(path, []byte(selfTest), 0o644); err != nil {
		return CheckResult{Name: "serve-spec", Output: err.Error()}
	}
	defer os.Remove(path)
	return runGo(ctx, dir, "serve-spec", "test", "-count=1", "-run", "^TestAutodocServesSpec$", "./"+strip.SpecDir+"/")
}
