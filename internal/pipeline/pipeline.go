// Package pipeline runs the deterministic steps and the LLM steps in order:
// fetch, index, strip, swagger agent, gate A, add route, docs agent, gate B,
// judge, commit. See DESIGN.md.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
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
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

// swaggerAttempts is how many times gate A may run before the run gives up:
// the first attempt plus two retries with the gate report fed back.
const swaggerAttempts = 3

// docsAttempts is how many times gate B and the judge may run: the first
// attempt plus one retry with each page's issues fed back to the session
// that wrote it.
const docsAttempts = 2

// Input is what a run is pointed at.
type Input struct {
	Repo     string // git URL or local path
	Main     string // file with the func main to document; found from the facts when empty
	RunTests bool   // also run the target's own tests; reported, never blocking
}

// Result is what a run leaves behind.
type Result struct {
	Repo      *repo.Repo
	FactsPath string
	Main      string          // file that holds the general API info
	Route     *specroute.Plan // where GET /openapi.json is registered
	SpecDir   string          // the generated swagger.json and swagger.yaml, once gate A is green
	Docs      []string        // the documentation pages, once gate B is green and the judge passed
	Judge     string          // the judge's report
}

// Baseline is the state of the working copy before any change: written to
// baseline.json and used by the gates to prove that code did not change.
type Baseline struct {
	Repo     *repo.Repo           `json:"repo"`
	Checks   []verify.CheckResult `json:"checks"`
	Snapshot guard.Snapshot       `json:"snapshot"`
}

// Run executes the pipeline against in.Repo. The working copy is cloned into
// the run directory, next to the trace.
func Run(ctx context.Context, cfg config.Config, rec *trace.Recorder, in Input) (*Result, error) {
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
		// Planned now so that a service the route template cannot handle
		// fails before the agents run.
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
			return annotate(ctx, cfg, rec, verify.GateAInput{
				Dir:        workDir,
				Main:       res.Main,
				Baseline:   snap,
				BaseChecks: baseChecks,
				RunTests:   in.RunTests,
				Original:   original,
				Facts:      facts,
			})
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
		err = step(rec, docs.Step, func() (string, error) {
			return document(ctx, cfg, rec, workDir, facts)
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
	return res, nil
}

// annotate runs the swagger agent and gate A, feeding each failed gate
// report back to the agent in the same session, at most swaggerAttempts times.
func annotate(ctx context.Context, cfg config.Config, rec *trace.Recorder, in verify.GateAInput) (string, error) {
	a, err := swagger.New(ctx, cfg, rec, in.Dir, in.Main, in.Facts)
	if err != nil {
		return "", err
	}
	if err := a.Annotate(ctx); err != nil {
		return "", err
	}
	for attempt := 1; ; attempt++ {
		start := time.Now()
		rep := verify.GateA(ctx, in)
		if err := writeJSON(filepath.Join(rec.Dir(), "gate_a.json"), rep); err != nil {
			return "", err
		}
		e := trace.Event{
			Type: trace.Gate, Step: "gate_a", LatencyMS: time.Since(start).Milliseconds(),
			Detail: fmt.Sprintf("attempt %d of %d: %d issue(s)", attempt, swaggerAttempts, len(rep.Issues)),
		}
		if !rep.OK() {
			e.Error = rep.String()
		}
		rec.Emit(e)
		if rep.OK() {
			return fmt.Sprintf("gate A green on attempt %d of %d", attempt, swaggerAttempts), nil
		}
		if attempt == swaggerAttempts {
			return "", fmt.Errorf("gate A still fails after %d attempts (report in gate_a.json):\n%s", attempt, rep)
		}
		rec.Emit(trace.Event{
			Type: trace.Retry, Step: swagger.Step,
			Detail: fmt.Sprintf("attempt %d of %d: %d gate A issue(s) sent back to the agent", attempt+1, swaggerAttempts, len(rep.Issues)),
		})
		if err := a.Fix(ctx, rep.String()); err != nil {
			return "", err
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

// document runs the docs agent, then gate B and the judge as one check:
// gate B's issues, or once it is green the claims the judge found
// unsupported, go back to the session that wrote each page, at most
// docsAttempts times. After a fix gate B runs again before the judge.
func document(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir string, facts *gofacts.Facts) (string, error) {
	before, err := guard.Take(dir)
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	// A page left by an earlier run would pass for one written now.
	for _, p := range verify.DocPages {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	a, err := docs.New(ctx, cfg, rec, dir, facts)
	if err != nil {
		return "", err
	}
	if err := a.Write(ctx); err != nil {
		return "", err
	}
	j, err := judge.New(ctx, cfg, rec)
	if err != nil {
		return "", err
	}
	for attempt := 1; ; attempt++ {
		start := time.Now()
		rep := verify.GateB(verify.GateBInput{Dir: dir, Facts: facts, Before: before})
		if err := writeJSON(filepath.Join(rec.Dir(), "gate_b.json"), rep); err != nil {
			return "", err
		}
		e := trace.Event{
			Type: trace.Gate, Step: "gate_b", LatencyMS: time.Since(start).Milliseconds(),
			Detail: fmt.Sprintf("attempt %d of %d: %d issue(s)", attempt, docsAttempts, len(rep.Issues)),
		}
		if !rep.OK() {
			e.Error = rep.String()
		}
		rec.Emit(e)
		check, file := "gate B", "gate_b.json"
		if rep.OK() {
			jrep, err := j.Check(ctx, dir, facts)
			if err != nil {
				return "", err
			}
			if err := writeJSON(filepath.Join(rec.Dir(), "judge.json"), jrep); err != nil {
				return "", err
			}
			rep = verify.Report{Gate: "judge", Issues: jrep.Issues()}
			e := trace.Event{
				Type: trace.Gate, Step: judge.Step, LatencyMS: jrep.DurationMS,
				Detail: fmt.Sprintf("attempt %d of %d: %s", attempt, docsAttempts, jrep.Summary()),
			}
			if !rep.OK() {
				e.Error = rep.String()
			}
			rec.Emit(e)
			if rep.OK() {
				return fmt.Sprintf("%d pages; gate B green and the judge passed on attempt %d of %d: %s",
					len(verify.DocPages), attempt, docsAttempts, jrep.Summary()), nil
			}
			check, file = "the judge", "judge.json"
		}
		if attempt == docsAttempts {
			return "", fmt.Errorf("%s still fails after %d attempts (report in %s):\n%s", check, attempt, file, rep)
		}
		byPage, err := issuesByPage(rep.Issues)
		if err != nil {
			return "", fmt.Errorf("%s: %w (report in %s):\n%s", check, err, file, rep)
		}
		rec.Emit(trace.Event{
			Type: trace.Retry, Step: docs.Step,
			Detail: fmt.Sprintf("attempt %d of %d: %d %s issue(s) on %d page(s) sent back to the agent", attempt+1, docsAttempts, len(rep.Issues), check, len(byPage)),
		})
		for _, p := range verify.DocPages {
			if is := byPage[p]; len(is) > 0 {
				if err := a.Fix(ctx, p, is); err != nil {
					return "", err
				}
			}
		}
	}
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
