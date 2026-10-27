// Package verify implements gate A (spec and code) and gate B (docs) as deterministic checks.
package verify

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxOutput keeps the tail of a failing command: enough to see the error,
// small enough to hand back to an agent.
const maxOutput = 4 << 10

// CheckResult is the outcome of one command run against the working copy.
type CheckResult struct {
	Name       string `json:"name"`
	Cmd        string `json:"cmd"`
	OK         bool   `json:"ok"`
	Advisory   bool   `json:"advisory,omitempty"` // reported, never blocks a run
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Checks runs the orchestrator's own checks in dir: go build and go vet,
// which also type-checks the test files without running them. The target's
// test suite runs only with runTests, as an advisory check: it may need a
// database, network or credentials the orchestrator does not have, and the
// code guard already proves that no code changed. They all run, even after a
// failure, so the report is complete.
func Checks(ctx context.Context, dir string, runTests bool) []CheckResult {
	out := []CheckResult{
		runGo(ctx, dir, "build", "build", "./..."),
		runGo(ctx, dir, "vet", "vet", "./..."),
	}
	if runTests {
		t := runGo(ctx, dir, "test", "test", "-count=1", "./...")
		t.Advisory = true
		out = append(out, t)
	}
	return out
}

// Regressions returns the failed checks in cur that count against a run: not
// advisory, and green in base, so a target that already failed a check
// before any change (an existing vet finding, say) can still be documented.
func Regressions(base, cur []CheckResult) []CheckResult {
	failedBefore := map[string]bool{}
	for _, b := range base {
		if !b.OK {
			failedBefore[b.Name] = true
		}
	}
	var out []CheckResult
	for _, c := range cur {
		if !c.OK && !c.Advisory && !failedBefore[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func runGo(ctx context.Context, dir, name string, args ...string) CheckResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	// The target is its own module; never let a go.work above it change the build.
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	res := CheckResult{
		Name:       name,
		Cmd:        "go " + strings.Join(args, " "),
		OK:         err == nil,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		text := strings.TrimSpace(out.String())
		if len(text) > maxOutput {
			text = "…" + text[len(text)-maxOutput:]
		}
		if text == "" {
			text = err.Error()
		}
		res.Output = text
	}
	return res
}
