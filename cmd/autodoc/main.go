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

	"autodoc/internal/config"
	"autodoc/internal/pipeline"
	"autodoc/internal/smoke"
	"autodoc/internal/trace"
)

const usage = `usage:
  autodoc run   --repo <url|path> [--runs-dir runs]   document a repository
  autodoc smoke [--dir .] [--runs-dir runs]           check the ADK + Gemini wiring with one tool call
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
	fmt.Println("run:", rec.RunID(), "trace:", rec.Dir())
	res, err := pipeline.Run(ctx, cfg, rec, pipeline.Input{Repo: *repo})
	if err != nil {
		return err
	}
	fmt.Println("working copy:", res.Repo.Dir, "branch:", res.Repo.Branch)
	fmt.Println("facts:", res.FactsPath)
	fmt.Println("stopped after index: the remaining steps are not implemented yet")
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

	answer, err := smoke.Run(ctx, cfg, rec, *dir)
	if err != nil {
		return err
	}
	t := rec.Totals()
	fmt.Printf("answer: %s\n", answer)
	fmt.Printf("model calls: %d, tool calls: %d, tokens in/out/total: %d/%d/%d\n",
		t.ModelCalls, t.ToolCalls, t.InputTokens, t.OutputTokens, t.TotalTokens)
	fmt.Println("trace:", rec.Dir())
	return nil
}
