package judge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/llm/llmtest"
	"autodoc/internal/trace"
)

// TestPromptPrefixIsStable: every model call, across claims, runs and pages,
// starts with the same system prompt, so providers can serve that start from
// their prompt cache.
func TestPromptPrefixIsStable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, d := range []string{"docs", "internal/shop"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "internal/shop/price.go"), []byte(priceGo), 0o644); err != nil {
		t.Fatal(err)
	}
	facts := &gofacts.Facts{Packages: []gofacts.Package{{Name: "shop", Dir: "internal/shop", Files: []string{"internal/shop/price.go"}}}}

	srv := llmtest.Start(t, `{"verdict": "supported", "reason": "matches"}`)
	cfg := config.Config{Provider: srv.Provider(), JudgeModels: []string{"m"}}
	for _, rules := range []string{
		"# Business rules\n\n- `internal/shop/price.go:Convert` converts an amount in cents to another currency.\n",
		"# Business rules\n\n- `internal/shop/price.go:Convert` divides by 100, then applies the EUR rate of 0.2.\n" +
			"- `internal/shop/price.go:Convert` returns a float64.\n",
	} {
		writeDocs(t, dir, rules)
		rec, err := trace.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rec.Close() })
		j, err := New(ctx, cfg, rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := j.Check(ctx, dir, facts); err != nil {
			t.Fatal(err)
		}
	}
	llmtest.CheckStablePrefix(t, srv.Requests())
}
