package llm

import (
	"context"
	"fmt"
	"iter"
	"testing"

	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"autodoc/internal/trace"
)

// fakeLLM fails with err (if set), otherwise answers with its own name.
type fakeLLM struct {
	name  string
	err   error
	calls int
	seen  []string // req.Model of each call
}

func (f *fakeLLM) Name() string { return f.name }

func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.calls++
		f.seen = append(f.seen, req.Model)
		if f.err != nil {
			yield(nil, fmt.Errorf("failed to call model: %w", f.err))
			return
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(f.name, genai.RoleModel)}, nil)
	}
}

func generate(t *testing.T, fb *Fallback) (string, error) {
	t.Helper()
	var text string
	for resp, err := range fb.GenerateContent(context.Background(), &model.LLMRequest{Model: fb.Name()}, false) {
		if err != nil {
			return "", err
		}
		text += resp.Content.Parts[0].Text
	}
	return text, nil
}

func newTestFallback(t *testing.T, models ...*fakeLLM) (*Fallback, *trace.Recorder) {
	t.Helper()
	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	llms := make([]model.LLM, len(models))
	for i, m := range models {
		llms[i] = m
	}
	fb, err := NewFallback(llms, rec)
	if err != nil {
		t.Fatal(err)
	}
	return fb, rec
}

func TestFallbackSwitchesAndSticks(t *testing.T) {
	a := &fakeLLM{name: "a", err: genai.APIError{Code: 503, Status: "UNAVAILABLE"}}
	b := &fakeLLM{name: "b"}
	fb, rec := newTestFallback(t, a, b)

	for range 2 {
		got, err := generate(t, fb)
		if err != nil || got != "b" {
			t.Fatalf("generate = %q, %v; want b", got, err)
		}
	}
	if a.calls != 1 || b.calls != 2 {
		t.Errorf("calls a=%d b=%d, want 1 and 2 (stays on b)", a.calls, b.calls)
	}
	if b.seen[0] != "b" {
		t.Errorf("b got req.Model %q, want b", b.seen[0])
	}
	if fb.Name() != "b" || rec.Totals().Fallbacks != 1 {
		t.Errorf("Name = %s, fallbacks = %d; want b and 1", fb.Name(), rec.Totals().Fallbacks)
	}
}

func TestFallbackOnRetiredModel(t *testing.T) {
	a := &fakeLLM{name: "a", err: &genai.APIError{Code: 404, Status: "NOT_FOUND"}}
	b := &fakeLLM{name: "b"}
	fb, _ := newTestFallback(t, a, b)
	if got, err := generate(t, fb); err != nil || got != "b" {
		t.Errorf("generate = %q, %v; want b", got, err)
	}
}

func TestShouldFallbackOnOpenAIStyleErrors(t *testing.T) {
	for code, want := range map[int]bool{404: true, 429: true, 503: true, 400: false, 401: false} {
		err := fmt.Errorf("wrapped: %w", &openai.Error{StatusCode: code})
		if got := shouldFallback(context.Background(), err); got != want {
			t.Errorf("shouldFallback(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestFallbackKeepsNonAvailabilityErrors(t *testing.T) {
	a := &fakeLLM{name: "a", err: genai.APIError{Code: 400, Status: "INVALID_ARGUMENT"}}
	b := &fakeLLM{name: "b"}
	fb, rec := newTestFallback(t, a, b)
	if _, err := generate(t, fb); err == nil {
		t.Fatal("want the 400 to reach the caller")
	}
	if b.calls != 0 || rec.Totals().Fallbacks != 0 {
		t.Errorf("b.calls = %d, fallbacks = %d; want 0 and 0", b.calls, rec.Totals().Fallbacks)
	}
}

func TestFallbackLastModelErrorReachesCaller(t *testing.T) {
	a := &fakeLLM{name: "a", err: genai.APIError{Code: 503}}
	b := &fakeLLM{name: "b", err: genai.APIError{Code: 503}}
	fb, rec := newTestFallback(t, a, b)
	if _, err := generate(t, fb); err == nil {
		t.Fatal("want an error when every model is unavailable")
	}
	if rec.Totals().Fallbacks != 1 {
		t.Errorf("fallbacks = %d, want 1", rec.Totals().Fallbacks)
	}
}
