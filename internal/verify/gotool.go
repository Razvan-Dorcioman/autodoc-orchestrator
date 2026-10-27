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
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// GoChecks runs go build, go vet and go test (uncached) in dir. They all run,
// even after a failure, so the report is complete.
func GoChecks(ctx context.Context, dir string) []CheckResult {
	return []CheckResult{
		runGo(ctx, dir, "build", "build", "./..."),
		runGo(ctx, dir, "vet", "vet", "./..."),
		runGo(ctx, dir, "test", "test", "-count=1", "./..."),
	}
}

// AllOK reports whether every check passed.
func AllOK(results []CheckResult) bool {
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
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
