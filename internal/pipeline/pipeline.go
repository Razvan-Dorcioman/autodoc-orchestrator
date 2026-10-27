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
	"time"

	"autodoc/internal/agents/swagger"
	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/guard"
	"autodoc/internal/repo"
	"autodoc/internal/strip"
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

// swaggerAttempts is how many times gate A may run before the run gives up:
// the first attempt plus two retries with the gate report fed back.
const swaggerAttempts = 3

// Input is what a run is pointed at.
type Input struct {
	Repo string // git URL or local path
}

// Result is what a run leaves behind.
type Result struct {
	Repo      *repo.Repo
	FactsPath string
	SpecDir   string // the generated swagger.json and swagger.yaml, once gate A is green
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
		checks := verify.GoChecks(ctx, workDir)
		for _, c := range checks {
			e := trace.Event{Type: trace.Gate, Step: "baseline", Detail: c.Cmd, LatencyMS: c.DurationMS}
			if !c.OK {
				e.Error = c.Output
			}
			rec.Emit(e)
		}
		if err := writeJSON(filepath.Join(rec.Dir(), "baseline.json"), Baseline{Repo: res.Repo, Checks: checks, Snapshot: snap}); err != nil {
			return "", err
		}
		if !verify.AllOK(checks) {
			// Without a green baseline, "still builds and passes after the run" proves nothing.
			return "", fmt.Errorf("the target does not build, vet or test cleanly before any change; see baseline.json")
		}
		return fmt.Sprintf("%d files fingerprinted; build, vet and test pass", len(snap.Files)), nil
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
		res.FactsPath = filepath.Join(rec.Dir(), "facts.json")
		if err := writeJSON(res.FactsPath, facts); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d routes, %d response sites, %d constants, %d types, %d env vars, %d tests",
			len(facts.Routes), len(facts.Responses), len(facts.Constants), len(facts.Types), len(facts.Env), len(facts.Tests)), nil
	})
	if err != nil {
		return res, err
	}

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
			return annotate(ctx, cfg, rec, workDir, snap, facts)
		})
	}
	if err != nil {
		if rErr := repo.Restore(ctx, workDir); rErr != nil {
			return res, fmt.Errorf("%w; restoring the working copy also failed: %v", err, rErr)
		}
		return res, fmt.Errorf("%w; the working copy was restored to the baseline", err)
	}
	res.SpecDir = filepath.Join(workDir, filepath.FromSlash(strip.SpecDir))
	return res, nil
}

// annotate runs the swagger agent and gate A, feeding each failed gate
// report back to the agent in the same session, at most swaggerAttempts times.
func annotate(ctx context.Context, cfg config.Config, rec *trace.Recorder, workDir string, snap guard.Snapshot, facts *gofacts.Facts) (string, error) {
	a, err := swagger.New(ctx, cfg, rec, workDir, facts)
	if err != nil {
		return "", err
	}
	if err := a.Annotate(ctx); err != nil {
		return "", err
	}
	in := verify.GateAInput{
		Dir:      workDir,
		Baseline: snap,
		Original: func(rel string) ([]byte, error) { return repo.Show(ctx, workDir, "HEAD", rel) },
		Facts:    facts,
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
