package docs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/llm/llmtest"
	"autodoc/internal/strip"
	"autodoc/internal/trace"
)

// TestPromptPrefixIsStable: every model call, across pages, runs and inputs,
// starts with the same system prompt and tools, so providers can serve that
// start from their prompt cache.
func TestPromptPrefixIsStable(t *testing.T) {
	ctx := context.Background()
	facts, err := gofacts.Extract(ctx, "../../gofacts/testdata/svc")
	if err != nil {
		t.Fatal(err)
	}
	facts.Routes = slices.DeleteFunc(facts.Routes, func(r gofacts.Route) bool { return r.HandlerPos == "" })
	one := *facts
	one.Routes = facts.Routes[:1]

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, strip.SpecDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strip.SpecDir, "swagger.json"), []byte(`{"swagger":"2.0","paths":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := llmtest.Start(t, "Done.")
	cfg := config.Config{Provider: srv.Provider(), AgentModels: []string{"m"}}
	for _, f := range []*gofacts.Facts{facts, &one} {
		rec, err := trace.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rec.Close() })
		a, err := New(ctx, cfg, rec, dir, f)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Write(ctx); err != nil {
			t.Fatal(err)
		}
	}
	llmtest.CheckStablePrefix(t, srv.Requests())
}
