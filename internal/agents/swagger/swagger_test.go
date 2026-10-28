package swagger

import (
	"context"
	"slices"
	"testing"

	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/llm/llmtest"
	"autodoc/internal/trace"
)

// TestPromptPrefixIsStable: every model call, across turns, runs and inputs,
// starts with the same system prompt and tools, so providers can serve that
// start from their prompt cache; and the cache hits they report reach the
// trace.
func TestPromptPrefixIsStable(t *testing.T) {
	ctx := context.Background()
	const dir = "../../gofacts/testdata/svc"
	facts, err := gofacts.Extract(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only the routes with handler source to annotate.
	facts.Routes = slices.DeleteFunc(facts.Routes, func(r gofacts.Route) bool { return r.HandlerPos == "" })
	one := *facts
	one.Routes = facts.Routes[:1]

	srv := llmtest.Start(t, "Done.")
	cfg := config.Config{Provider: srv.Provider(), AgentModels: []string{"m"}}
	for _, f := range []*gofacts.Facts{facts, &one} {
		rec, err := trace.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rec.Close() })
		a, err := New(ctx, cfg, rec, dir, "cmd/server/main.go", f)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Annotate(ctx); err != nil {
			t.Fatal(err)
		}
		tot := rec.Totals()
		if want := tot.ModelCalls * llmtest.CachedTokens; tot.ModelCalls == 0 || tot.CachedTokens != want {
			t.Errorf("%d model calls with %d cached tokens, want %d", tot.ModelCalls, tot.CachedTokens, want)
		}
	}
	llmtest.CheckStablePrefix(t, srv.Requests())
}
