// Package pipeline runs the deterministic steps and the LLM steps in order:
// fetch, index, strip, swagger agent, gate A, add route, docs agent, gate B,
// judge, commit. See DESIGN.md.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"autodoc/internal/agents/docs"
	"autodoc/internal/agents/judge"
	"autodoc/internal/agents/swagger"
	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/repo"
	"autodoc/internal/specroute"
	"autodoc/internal/strip"
	"autodoc/internal/tools"
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

// swaggerAttempts is how many times gate A may run before the run gives up:
// the first attempt plus two retries with the gate report fed back.
const swaggerAttempts = 3

// gateBAttempts is how many times gate B may run in a row before the run
// gives up: the first attempt plus one retry with each page's issues fed back
// to the session that wrote it. It starts over after each judge fix.
const gateBAttempts = 2

// judgeAttempts is how many judge passes the docs get: the first, then one
// after its unsupported claims went back to the pages. The last pass is
// advisory: the judge is a model and can be wrong, so what it still finds
// is reported and committed for review instead of failing the run.
const judgeAttempts = 2

// Input is what a run is pointed at.
type Input struct {
	Repo     string // git URL or local path
	Main     string // file with the func main to document; found from the facts when empty
	RunTests bool   // also run the target's own tests; reported, never blocking
}

// Result is what a run leaves behind.
type Result struct {
	Repo      *repo.Repo
	FactsPath string          // facts.json, then facts_annotated.json: the facts the docs are written from
	Main      string          // file that holds the general API info
	Route     *specroute.Plan // where GET /openapi.json is registered
	SpecDir   string          // the generated swagger.json and swagger.yaml, once gate A is green
	Docs      []string        // the documentation pages, once gate B is green and the judge has had its passes
	Judge     string          // the judge's report
	Unchecked []judge.Result  // claims the judge's last pass still found unsupported, committed for review
	SpecGaps  []verify.Issue  // param constraints gate A's last attempt still found missing or wrong, committed for review
	Commit    string          // the run's commit on Repo.Branch; empty when nothing changed
	Files     []string        // the paths it changed
	PushedTo  string          // origin's URL, once the branch is pushed
}

// Outcome is what summary.json records of the result.
type Outcome struct {
	Source      string   `json:"source,omitempty"`
	BaseSHA     string   `json:"base_sha,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	Files       []string `json:"files,omitempty"`
	PushedTo    string   `json:"pushed_to,omitempty"`
	WorkDir     string   `json:"work_dir,omitempty"`
	Unsupported []string `json:"unsupported_claims,omitempty"` // the judge's last pass, advisory: "page:line: reason"
	SpecGaps    []string `json:"spec_constraints,omitempty"`   // gate A's last attempt, advisory: "where: message"
}

// issueLines renders the leftover constraint issues as "where: message".
func issueLines(issues []verify.Issue) []string {
	var out []string
	for _, is := range issues {
		out = append(out, is.Where+": "+is.Message)
	}
	return out
}

// claimLines renders the leftover claims as "page:line: reason".
func claimLines(claims []judge.Result) []string {
	var out []string
	for _, c := range claims {
		out = append(out, fmt.Sprintf("%s:%d: %s", c.Page, c.Line, c.Reason))
	}
	return out
}

func (r *Result) outcome() Outcome {
	o := Outcome{Commit: r.Commit, Files: r.Files, PushedTo: r.PushedTo, Unsupported: claimLines(r.Unchecked), SpecGaps: issueLines(r.SpecGaps)}
	if r.Repo != nil {
		o.Source, o.BaseSHA, o.Branch, o.WorkDir = r.Repo.Source, r.Repo.BaseSHA, r.Repo.Branch, r.Repo.Dir
	}
	return o
}

// Baseline is the state of the working copy before any change: written to
// baseline.json and used by the gates to prove that code did not change.
type Baseline struct {
	Repo     *repo.Repo           `json:"repo"`
	Checks   []verify.CheckResult `json:"checks"`
	Snapshot guard.Snapshot       `json:"snapshot"`
}

// Run executes the pipeline against in.Repo. The working copy is cloned into
// the run directory, next to the trace. How the run ended goes to the
// recorder, for summary.json.
func Run(ctx context.Context, cfg config.Config, rec *trace.Recorder, in Input) (*Result, error) {
	res, err := run(ctx, cfg, rec, in)
	status := trace.Committed
	switch {
	case err != nil:
		status = trace.Failed
	case res.Commit == "":
		status = trace.Unchanged
	}
	rec.Finish(status, res.outcome())
	return res, err
}

func run(ctx context.Context, cfg config.Config, rec *trace.Recorder, in Input) (*Result, error) {
	res := &Result{}
	workDir := filepath.Join(rec.Dir(), "work")
	var snap guard.Snapshot
	var baseChecks []verify.CheckResult
	var facts *gofacts.Facts

	err := step(rec, "fetch", func() (string, error) {
		r, err := repo.Prepare(ctx, in.Repo, workDir)
		if err != nil {
			return "", err
		}
		res.Repo = r
		return fmt.Sprintf("cloned %s at %s onto branch %s", r.Source, r.BaseSHA, r.Branch), nil
	})
	if err != nil {
		return res, err
	}

	err = step(rec, "baseline", func() (string, error) {
		var err error
		snap, err = guard.Take(workDir)
		if err != nil {
			return "", fmt.Errorf("snapshot: %w", err)
		}
		baseChecks = verify.Checks(ctx, workDir, in.RunTests)
		if err := writeJSON(filepath.Join(rec.Dir(), "baseline.json"), Baseline{Repo: res.Repo, Checks: baseChecks, Snapshot: snap}); err != nil {
			return "", err
		}
		var status []string
		for _, c := range baseChecks {
			e := trace.Event{Type: trace.Gate, Step: "baseline", Detail: c.Cmd, LatencyMS: c.DurationMS}
			result := "ok"
			if !c.OK {
				e.Error, result = c.Output, "fails"
			}
			rec.Emit(e)
			status = append(status, c.Name+" "+result)
			// Facts need type-checked code and swag needs it to parse. Every
			// other check only has to stay as green as it is now.
			if c.Name == "build" && !c.OK {
				return "", fmt.Errorf("the target does not build before any change; see baseline.json")
			}
		}
		return fmt.Sprintf("%d files fingerprinted; %s", len(snap.Files), strings.Join(status, ", ")), nil
	})
	if err != nil {
		return res, err
	}

	err = step(rec, "index", func() (string, error) {
		var err error
		facts, err = gofacts.Extract(ctx, workDir)
		if err != nil {
			return "", err
		}
		// A working copy that went through a run already serves the spec;
		// that route is the template's, not one for the agents to document.
		facts.Routes = slices.DeleteFunc(facts.Routes, specroute.Own)
		res.FactsPath = filepath.Join(rec.Dir(), "facts.json")
		if err := writeJSON(res.FactsPath, facts); err != nil {
			return "", err
		}
		if res.Main, err = entrypoint(facts, in.Main); err != nil {
			return "", err
		}
		// Checked and planned now so that a service with a spec of its own,
		// or one the route template cannot handle, fails before the agents run.
		if err := specroute.CheckExisting(workDir); err != nil {
			return "", err
		}
		plan, err := specroute.PlanFor(workDir, facts)
		if err != nil {
			return "", fmt.Errorf("spec route: %w", err)
		}
		res.Route = &plan
		return fmt.Sprintf("%d routes, %d response sites, %d constants, %d types, %d env vars, %d tests; general API info on %s; spec route: %s",
			len(facts.Routes), len(facts.Responses), len(facts.Constants), len(facts.Types), len(facts.Env), len(facts.Tests), res.Main, plan), nil
	})
	if err != nil {
		return res, err
	}

	original := func(rel string) ([]byte, error) { return repo.Show(ctx, workDir, "HEAD", rel) }

	// From here on the working copy changes; on failure it goes back to the baseline.
	err = step(rec, "strip", func() (string, error) {
		r, err := strip.Run(workDir)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("removed %d annotation lines from %d files; deleted %d generated spec files", r.Lines, r.Files, len(r.Deleted)), nil
	})
	if err == nil {
		err = step(rec, swagger.Step, func() (string, error) {
			detail, left, err := annotate(ctx, cfg, rec, verify.GateAInput{
				Dir:        workDir,
				Main:       res.Main,
				Baseline:   snap,
				BaseChecks: baseChecks,
				RunTests:   in.RunTests,
				Original:   original,
				Facts:      facts,
			})
			res.SpecGaps = left
			return detail, err
		})
	}
	if err == nil {
		err = step(rec, "add_route", func() (string, error) {
			return addRoute(ctx, rec, verify.RouteInput{
				Dir:        workDir,
				Plan:       *res.Route,
				Baseline:   snap,
				BaseChecks: baseChecks,
				RunTests:   in.RunTests,
				Original:   original,
			})
		})
	}
	if err == nil {
		// The annotations moved every line below them, so the docs, gate B and
		// the judge need the positions of the code as it is now.
		err = step(rec, "reindex", func() (string, error) {
			path := filepath.Join(rec.Dir(), "facts_annotated.json")
			facts, err = reindex(ctx, workDir, facts, path)
			if err != nil {
				return "", err
			}
			res.FactsPath = path
			return fmt.Sprintf("%d routes at their annotated positions", len(facts.Routes)), nil
		})
	}
	if err == nil {
		err = step(rec, docs.Step, func() (string, error) {
			detail, left, err := document(ctx, cfg, rec, workDir, facts)
			res.Unchecked = left
			return detail, err
		})
	}
	if err == nil {
		err = step(rec, "commit", func() (string, error) {
			return commit(ctx, rec, res)
		})
	}
	if err != nil {
		if rErr := repo.Restore(ctx, workDir); rErr != nil {
			return res, fmt.Errorf("%w; restoring the working copy also failed: %v", err, rErr)
		}
		return res, fmt.Errorf("%w; the working copy was restored to the baseline", err)
	}
	res.SpecDir = filepath.Join(workDir, filepath.FromSlash(strip.SpecDir))
	for _, p := range verify.DocPages {
		res.Docs = append(res.Docs, filepath.Join(workDir, filepath.FromSlash(p)))
	}
	res.Judge = filepath.Join(rec.Dir(), "judge.json")

	// The commit stays on the local branch whatever happens here.
	if cfg.GitToken != "" && res.Commit != "" {
		err = step(rec, "push", func() (string, error) {
			url, err := repo.Push(ctx, workDir, res.Repo.Branch, cfg.GitToken)
			if err != nil {
				return "", err
			}
			res.PushedTo = url
			return fmt.Sprintf("pushed %s to %s", res.Repo.Branch, url), nil
		})
	}
	return res, err
}

// commit commits everything the run changed as one commit on the run branch,
// after checking once more that every changed path is one a run may change:
// a .go file (gate A proved only comment lines and the route changed), the
// spec package or a docs page.
func commit(ctx context.Context, rec *trace.Recorder, res *Result) (string, error) {
	allowed := func(p string) bool {
		return strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") ||
			strings.HasPrefix(p, strip.SpecDir+"/") || slices.Contains(verify.DocPages, p)
	}
	sha, files, err := repo.Commit(ctx, res.Repo.Dir, commitMessage(rec, res), allowed)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "nothing to commit: the annotations, spec and docs equal the base commit's", nil
	}
	res.Commit, res.Files = sha, files
	return fmt.Sprintf("committed %d files as %s on %s", len(files), sha[:min(12, len(sha))], res.Repo.Branch), nil
}

func commitMessage(rec *trace.Recorder, res *Result) string {
	models := slices.Sorted(maps.Keys(rec.Summary().Models))
	var b strings.Builder
	fmt.Fprintf(&b, `Add API annotations, OpenAPI spec and docs

Generated by autodoc from %s: swaggo annotations on the route
handlers and on func main in %s, the OpenAPI spec in %s served at
%s, and the pages in %s. In existing Go code only comment lines
changed, apart from the line that registers that route.
`, res.Repo.BaseSHA, res.Main, strip.SpecDir, specroute.Pattern, tools.DocsDir)
	if len(res.Unchecked) > 0 {
		fmt.Fprintf(&b, "\nTo review: the fact-check's last pass still found %d claim(s)\nunsupported by the code they cite (details in the run's judge.json):\n", len(res.Unchecked))
		for _, l := range claimLines(res.Unchecked) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	if len(res.SpecGaps) > 0 {
		fmt.Fprintf(&b, "\nTo review: gate A's last attempt still found %d param constraint(s)\nmissing from the spec or different from the code (details in the run's gate_a.json):\n", len(res.SpecGaps))
		for _, l := range issueLines(res.SpecGaps) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	fmt.Fprintf(&b, "\nRun: %s\nModels: %s\n", rec.RunID(), strings.Join(models, ", "))
	return b.String()
}

// annotate runs the swagger agent and gate A, feeding each failed gate
// report back to the agent in the same session, at most swaggerAttempts times.
// On the last attempt, constraint issues alone do not fail the run: they are
// returned, for the commit and the summary.
func annotate(ctx context.Context, cfg config.Config, rec *trace.Recorder, in verify.GateAInput) (string, []verify.Issue, error) {
	a, err := swagger.New(ctx, cfg, rec, in.Dir, in.Main, in.Facts)
	if err != nil {
		return "", nil, err
	}
	if err := a.Annotate(ctx); err != nil {
		return "", nil, err
	}
	for attempt := 1; ; attempt++ {
		start := time.Now()
		rep := verify.GateA(ctx, in)
		last := attempt == swaggerAttempts
		rep.Advisory = last && !rep.OK() && len(rep.Blocking()) == 0
		if err := writeJSON(filepath.Join(rec.Dir(), "gate_a.json"), rep); err != nil {
			return "", nil, err
		}
		e := trace.Event{
			Type: trace.Gate, Step: "gate_a", LatencyMS: time.Since(start).Milliseconds(),
			Detail: fmt.Sprintf("attempt %d of %d: %d issue(s)", attempt, swaggerAttempts, len(rep.Issues)),
		}
		if rep.Advisory {
			e.Detail += "; advisory: only param constraints are left, the run goes on and reports them"
		}
		if !rep.OK() {
			e.Error = rep.String()
		}
		rec.Emit(e)
		switch {
		case rep.OK():
			return fmt.Sprintf("gate A green on attempt %d of %d", attempt, swaggerAttempts), nil, nil
		case rep.Advisory:
			return fmt.Sprintf("gate A green on attempt %d of %d but for %d param constraint(s), kept for review (gate_a.json)",
				attempt, swaggerAttempts, len(rep.Issues)), rep.Issues, nil
		case last:
			return "", nil, fmt.Errorf("gate A still fails after %d attempts (report in gate_a.json):\n%s", attempt, rep)
		}
		rec.Emit(trace.Event{
			Type: trace.Retry, Step: swagger.Step,
			Detail: fmt.Sprintf("attempt %d of %d: %d gate A issue(s) sent back to the agent", attempt+1, swaggerAttempts, len(rep.Issues)),
		})
		if err := a.Fix(ctx, rep.String()); err != nil {
			return "", nil, err
		}
	}
}

// addRoute writes the spec package, registers GET /openapi.json and checks
// the result. The edit is a template, so a failed check is a bug or a
// service the template does not fit: there is no agent to retry with.
func addRoute(ctx context.Context, rec *trace.Recorder, in verify.RouteInput) (string, error) {
	if err := specroute.Apply(in.Dir, in.Plan); err != nil {
		return "", err
	}
	start := time.Now()
	rep := verify.Route(ctx, in)
	if err := writeJSON(filepath.Join(rec.Dir(), "route.json"), rep); err != nil {
		return "", err
	}
	e := trace.Event{
		Type: trace.Gate, Step: "route", LatencyMS: time.Since(start).Milliseconds(),
		Detail: fmt.Sprintf("%d issue(s)", len(rep.Issues)),
	}
	if !rep.OK() {
		e.Error = rep.String()
	}
	rec.Emit(e)
	if !rep.OK() {
		return "", fmt.Errorf("the spec route check failed (report in route.json):\n%s", rep)
	}
	return fmt.Sprintf("%s; served from %s", in.Plan, specroute.File), nil
}

// document runs the docs agent, then gate B and the judge, each with its own
// attempts. Gate B's issues go back to the session that wrote each page, at
// most gateBAttempts runs in a row, and it blocks: a page it rejects fails
// the run. Once it is green the judge runs; its unsupported claims go back
// the same way, and after the fix gate B must pass again before the next
// judge pass. The last of judgeAttempts passes is advisory: the claims it
// still finds unsupported are returned, for the commit and the summary.
func document(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir string, facts *gofacts.Facts) (string, []judge.Result, error) {
	before, err := guard.Take(dir)
	if err != nil {
		return "", nil, fmt.Errorf("snapshot: %w", err)
	}
	// A page left by an earlier run would pass for one written now.
	for _, p := range verify.DocPages {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	a, err := docs.New(ctx, cfg, rec, dir, facts)
	if err != nil {
		return "", nil, err
	}
	if err := a.Write(ctx); err != nil {
		return "", nil, err
	}
	j, err := judge.New(ctx, cfg, rec)
	if err != nil {
		return "", nil, err
	}

	// fix sends a check's issues back to the sessions that wrote the pages.
	fix := func(check, next string, issues []verify.Issue) error {
		byPage, err := issuesByPage(issues)
		if err != nil {
			return fmt.Errorf("%s: %w:\n%s", check, err, verify.Report{Gate: check, Issues: issues})
		}
		rec.Emit(trace.Event{
			Type: trace.Retry, Step: docs.Step,
			Detail: fmt.Sprintf("%s: %d %s issue(s) on %d page(s) sent back to the agent", next, len(issues), check, len(byPage)),
		})
		for _, p := range verify.DocPages {
			if is := byPage[p]; len(is) > 0 {
				if err := a.Fix(ctx, p, is); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// gateB runs gate B until it is green, at most gateBAttempts times. Before
	// each run, links to code become citations: a mechanical fix, not worth
	// an attempt.
	gateB := func() error {
		for attempt := 1; ; attempt++ {
			n, err := verify.RewriteCodeLinks(dir)
			if err != nil {
				return fmt.Errorf("rewrite links to code: %w", err)
			}
			if len(n) > 0 {
				rec.Emit(trace.Event{Type: trace.Fixup, Step: "gate_b", Detail: verify.RewriteSummary(n)})
			}
			start := time.Now()
			rep := verify.GateB(verify.GateBInput{Dir: dir, Facts: facts, Before: before})
			if err := writeJSON(filepath.Join(rec.Dir(), "gate_b.json"), rep); err != nil {
				return err
			}
			e := trace.Event{
				Type: trace.Gate, Step: "gate_b", LatencyMS: time.Since(start).Milliseconds(),
				Detail: fmt.Sprintf("attempt %d of %d: %d issue(s)", attempt, gateBAttempts, len(rep.Issues)),
			}
			if !rep.OK() {
				e.Error = rep.String()
			}
			rec.Emit(e)
			if rep.OK() {
				return nil
			}
			if attempt == gateBAttempts {
				return fmt.Errorf("gate B still fails after %d attempts (report in gate_b.json):\n%s", attempt, rep)
			}
			if err := fix("gate B", fmt.Sprintf("gate B attempt %d of %d", attempt+1, gateBAttempts), rep.Issues); err != nil {
				return err
			}
		}
	}

	for pass := 1; ; pass++ {
		if err := gateB(); err != nil {
			return "", nil, err
		}
		jrep, err := j.Check(ctx, dir, facts)
		if err != nil {
			return "", nil, err
		}
		last := pass == judgeAttempts
		jrep.Advisory = last && !jrep.OK()
		if err := writeJSON(filepath.Join(rec.Dir(), "judge.json"), jrep); err != nil {
			return "", nil, err
		}
		issues := jrep.Issues()
		e := trace.Event{
			Type: trace.Gate, Step: judge.Step, LatencyMS: jrep.DurationMS,
			Detail: fmt.Sprintf("pass %d of %d: %s", pass, judgeAttempts, jrep.Summary()),
		}
		if jrep.Advisory {
			e.Detail += "; advisory: the run goes on and reports them"
		}
		if len(issues) > 0 {
			e.Error = verify.Report{Gate: "judge", Issues: issues}.String()
		}
		rec.Emit(e)
		switch {
		case jrep.OK():
			return fmt.Sprintf("%d pages; gate B green and the judge passed on pass %d of %d: %s",
				len(verify.DocPages), pass, judgeAttempts, jrep.Summary()), nil, nil
		case last:
			left := jrep.Unsupported()
			return fmt.Sprintf("%d pages; gate B green; the judge's last pass still finds %d claim(s) unsupported, kept for review (judge.json): %s",
				len(verify.DocPages), len(left), jrep.Summary()), left, nil
		}
		if err := fix("the judge", fmt.Sprintf("judge pass %d of %d", pass+1, judgeAttempts), issues); err != nil {
			return "", nil, err
		}
	}
}

// reindex extracts the facts again from the annotated working copy and
// writes them to path. Only comments changed, so the routes must be the
// same ones, by pattern and handler, as before; only positions may differ.
func reindex(ctx context.Context, dir string, before *gofacts.Facts, path string) (*gofacts.Facts, error) {
	f, err := gofacts.Extract(ctx, dir)
	if err != nil {
		return nil, err
	}
	f.Routes = slices.DeleteFunc(f.Routes, specroute.Own)
	key := func(rs []gofacts.Route) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Pattern+" "+r.Handler)
		}
		return out
	}
	if a, b := key(before.Routes), key(f.Routes); !slices.Equal(a, b) {
		return nil, fmt.Errorf("the routes changed while only comments did: %v before, %v now", a, b)
	}
	return f, writeJSON(path, f)
}

// issuesByPage groups issues by the page their Where names, so each goes
// back to the session that wrote the page. An issue elsewhere is one the
// docs agent cannot fix.
func issuesByPage(issues []verify.Issue) (map[string][]verify.Issue, error) {
	byPage := map[string][]verify.Issue{}
	for _, is := range issues {
		page, _, _ := strings.Cut(is.Where, ":")
		if !slices.Contains(verify.DocPages, page) {
			return nil, fmt.Errorf("an issue at %q is outside the pages, which the docs agent cannot fix", is.Where)
		}
		byPage[page] = append(byPage[page], is)
	}
	return byPage, nil
}

// entrypoint is the file with the func main that gets the general API info:
// override when set, which must be one of the module's main packages, or
// else the one command that serves the routes.
func entrypoint(f *gofacts.Facts, override string) (string, error) {
	if override == "" {
		return f.Entrypoint()
	}
	override = filepath.ToSlash(filepath.Clean(override))
	var files []string
	for _, m := range f.Mains {
		if m.File == override {
			return override, nil
		}
		files = append(files, m.File)
	}
	return "", fmt.Errorf("--main %s: no func main there; the module's commands are in %s", override, strings.Join(files, ", "))
}

// step wraps fn in step_start / step_end trace events.
func step(rec *trace.Recorder, name string, fn func() (string, error)) error {
	rec.Emit(trace.Event{Type: trace.StepStart, Step: name})
	detail, err := fn()
	end := trace.Event{Type: trace.StepEnd, Step: name, Detail: detail}
	if err != nil {
		end.Error = err.Error()
	}
	rec.Emit(end)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
