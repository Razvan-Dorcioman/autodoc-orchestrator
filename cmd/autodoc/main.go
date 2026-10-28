// Command autodoc points at a Go service and produces swaggo annotations, a
// generated OpenAPI spec and project documentation, without changing what the
// service does. See DESIGN.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"autodoc/internal/config"
	"autodoc/internal/pipeline"
	"autodoc/internal/smoke"
	"autodoc/internal/trace"
)

const usage = `usage:
  autodoc run   --repo <url|path> [--main <file>] [--run-tests] [--runs-dir runs]
                                                      document a repository
  autodoc smoke [--dir .] [--runs-dir runs]           check the ADK + model wiring with one tool call
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "run":
		err = cmdRun(ctx, args[1:])
	case "smoke":
		err = cmdSmoke(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "", "git URL or local path of the service to document")
	mainFile := fs.String("main", "", "file with the func main to document, relative to the repo root (default: the one command that serves the routes)")
	runTests := fs.Bool("run-tests", false, "also run the repository's own tests and report the result; never blocks the run")
	runsDir := fs.String("runs-dir", "runs", "directory for run traces and summaries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" {
		return errors.New("--repo is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rec, err := trace.New(*runsDir)
	if err != nil {
		return err
	}
	defer rec.Close()
	rec.SetPricing(cfg.Pricing)
	fmt.Println("run:", rec.RunID(), "trace:", rec.Dir())
	res, err := pipeline.Run(ctx, cfg, rec, pipeline.Input{Repo: *repo, Main: *mainFile, RunTests: *runTests})
	t := rec.Totals()
	defer fmt.Println(costLine(rec.Summary().Cost))
	defer fmt.Printf("model calls: %d, tool calls: %d (%d failed), tokens in/out/total: %d/%d/%d (%d input cached)\n",
		t.ModelCalls, t.ToolCalls, t.ToolErrors, t.InputTokens, t.OutputTokens, t.TotalTokens, t.CachedTokens)
	defer fmt.Println("status:", rec.Summary().Status, "summary:", filepath.Join(rec.Dir(), "summary.json"))
	if res.Commit != "" {
		// A failed push leaves the commit on the local branch.
		fmt.Printf("commit: %s on %s in %s (%d files)\n", res.Commit, res.Repo.Branch, res.Repo.Dir, len(res.Files))
	}
	if err != nil {
		return err
	}
	fmt.Println("facts:", res.FactsPath)
	fmt.Println("general API info:", res.Main)
	fmt.Println("spec:", res.SpecDir)
	fmt.Println("spec route:", res.Route)
	fmt.Println("docs:", filepath.Dir(res.Docs[0]))
	fmt.Println("judge:", res.Judge)
	if n := len(res.Unchecked); n > 0 {
		fmt.Printf("to review: the judge's last pass still finds %d claim(s) unsupported; they are committed and listed in judge.json and the commit message\n", n)
	}
	switch {
	case res.PushedTo != "":
		fmt.Println("pushed:", res.Repo.Branch, "to", res.PushedTo)
	case res.Commit == "":
		fmt.Println("nothing to commit: the result equals the base commit")
	default:
		fmt.Println("not pushed: the branch is only in", res.Repo.Dir, "- set AUTODOC_GIT_TOKEN to push it to", res.Repo.Source)
	}
	return nil
}

func cmdSmoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory the read_file tool is scoped to; must contain go.mod")
	runsDir := fs.String("runs-dir", "runs", "directory for run traces and summaries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	rec, err := trace.New(*runsDir)
	if err != nil {
		return err
	}
	defer rec.Close()
	rec.SetPricing(cfg.Pricing)

	answer, err := smoke.Run(ctx, cfg, rec, *dir)
	if err != nil {
		return err
	}
	t := rec.Totals()
	fmt.Printf("answer: %s\n", answer)
	fmt.Printf("model calls: %d, tool calls: %d, tokens in/out/total: %d/%d/%d (%d input cached)\n",
		t.ModelCalls, t.ToolCalls, t.InputTokens, t.OutputTokens, t.TotalTokens, t.CachedTokens)
	fmt.Println(costLine(rec.Summary().Cost))
	fmt.Println("trace:", rec.Dir())
	return nil
}

// costLine is the estimated cost for the console; summary.json has the details.
func costLine(c trace.CostSummary) string {
	return fmt.Sprintf("estimated cost: $%.4f (free models count as $0); at the reference price (%s, %s): $%.4f",
		c.USD, c.ReferenceModel, c.ReferencePrice, c.ReferenceUSD)
}
