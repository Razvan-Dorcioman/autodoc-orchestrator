package llm

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"autodoc/internal/trace"
)

// Fallback is a model.LLM over a chain of models. When the active model fails
// with an availability error (after the HTTP retries), it switches to the next
// model in the chain and stays there for the rest of the run. Every switch is
// recorded in the trace.
type Fallback struct {
	models []model.LLM
	rec    *trace.Recorder

	mu     sync.Mutex
	active int
}

// NewFallback returns a Fallback that starts with models[0].
func NewFallback(models []model.LLM, rec *trace.Recorder) (*Fallback, error) {
	if len(models) == 0 {
		return nil, errors.New("fallback needs at least one model")
	}
	return &Fallback{models: models, rec: rec}, nil
}

// Name is the model currently in use. ADK copies it into each request.
func (f *Fallback) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models[f.active].Name()
}

func (f *Fallback) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.mu.Lock()
		start := f.active
		f.mu.Unlock()

		for i := start; i < len(f.models); i++ {
			m := f.models[i]
			r := *req
			r.Model = m.Name()

			var failed error
			yielded := false
			for resp, err := range m.GenerateContent(ctx, &r, stream) {
				// Only switch before anything reached the caller; a half-streamed answer cannot be replayed.
				if err != nil && !yielded && i < len(f.models)-1 && shouldFallback(ctx, err) {
					failed = err
					break
				}
				yielded = true
				if !yield(resp, err) {
					return
				}
			}
			if failed == nil {
				return
			}

			next := f.models[i+1].Name()
			f.mu.Lock()
			f.active = i + 1
			f.mu.Unlock()
			f.rec.Emit(trace.Event{
				Type:   trace.Fallback,
				Model:  m.Name(),
				Detail: fmt.Sprintf("switching to %s for the rest of the run", next),
				Error:  failed.Error(),
			})
		}
	}
}

// shouldFallback reports whether another model could succeed where this one
// failed: overload, rate limits, timeouts and retired models. Bad requests and
// auth errors would fail the same way on any model.
func shouldFallback(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false // the run itself was cancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	code := 0
	var apiErr genai.APIError
	var apiErrPtr *genai.APIError
	switch {
	case errors.As(err, &apiErr):
		code = apiErr.Code
	case errors.As(err, &apiErrPtr):
		code = apiErrPtr.Code
	}
	return code == http.StatusNotFound || retryable(code)
}
