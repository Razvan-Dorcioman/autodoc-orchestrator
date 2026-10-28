package llm

import (
	"errors"
	"strings"
	"testing"

	"autodoc/internal/trace"
)

// TestHooksStopAtTheRunTokenBudget: the budget counts every agent's calls,
// as the trace totals them, so a second agent stops once the first used it up.
func TestHooksStopAtTheRunTokenBudget(t *testing.T) {
	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	docs := &Hooks{Rec: rec, Step: "docs", TokenBudget: 100}
	judge := &Hooks{Rec: rec, Step: "judge", TokenBudget: 100}

	if _, err := docs.beforeModel(nil, nil); err != nil {
		t.Fatalf("under budget: %v", err)
	}
	rec.Emit(trace.Event{Type: trace.ModelCall, Step: "docs", TotalTokens: 120})

	_, err = judge.beforeModel(nil, nil)
	if !errors.Is(err, ErrTokenBudget) || !strings.Contains(err.Error(), "judge: run token budget exceeded: 120 of 100 tokens used") {
		t.Errorf("over budget: err = %v", err)
	}
	if _, err := (&Hooks{Rec: rec, Step: "s"}).beforeModel(nil, nil); err != nil {
		t.Errorf("no budget: err = %v, want none", err)
	}
}
