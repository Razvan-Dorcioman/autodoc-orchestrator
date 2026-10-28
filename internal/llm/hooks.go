// Package llm wires models (Gemini or a local OpenAI-compatible server) and ADK
// agent callbacks into the run trace.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"autodoc/internal/trace"
)

// Provider kinds.
const (
	ProviderGemini = "gemini" // Gemini through AI Studio
	ProviderLocal  = "local"  // open-weight model behind an OpenAI-compatible server (Ollama, llama.cpp, LM Studio, vLLM)
)

// Provider says which API serves the models and how to reach it.
type Provider struct {
	Kind    string
	APIKey  string        // required for gemini; optional for local servers
	BaseURL string        // local only, e.g. http://localhost:11434/v1
	Timeout time.Duration // per HTTP attempt; zero means the default
}

// NewModel returns a model from p. Transient API errors are retried with
// backoff, and each retry is recorded in rec.
func NewModel(ctx context.Context, name string, p Provider, rec *trace.Recorder) (model.LLM, error) {
	client := &http.Client{Transport: newRetryTransport(rec, name, p.Timeout)}
	var (
		m   model.LLM
		err error
	)
	switch p.Kind {
	case ProviderGemini:
		m, err = gemini.NewModel(ctx, name, &genai.ClientConfig{
			APIKey:     p.APIKey,
			Backend:    genai.BackendGeminiAPI,
			HTTPClient: client,
		})
	case ProviderLocal:
		// Chat Completions, not the Responses API: that is what local servers implement.
		m, err = openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{
			APIKey:     p.APIKey,
			BaseURL:    p.BaseURL,
			HTTPClient: client,
			API:        openaimodel.APIChatCompletions,
		})
		if err == nil {
			m = emptyOutputModel{m}
		}
	default:
		return nil, fmt.Errorf("unknown model provider %q", p.Kind)
	}
	if err != nil {
		return nil, fmt.Errorf("create model %s: %w", name, err)
	}
	return m, nil
}

// NewModelChain returns the models in names, in order, behind a Fallback.
func NewModelChain(ctx context.Context, names []string, p Provider, rec *trace.Recorder) (*Fallback, error) {
	models := make([]model.LLM, 0, len(names))
	for _, name := range names {
		m, err := NewModel(ctx, name, p, rec)
		if err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return NewFallback(models, rec)
}

// ErrTokenBudget ends an agent's run once the whole run has used its token
// budget.
var ErrTokenBudget = errors.New("run token budget exceeded")

// Hooks records one agent's model and tool calls in the trace and stops the
// agent once it has made MaxCalls model calls, or once the run, every agent
// together, has used TokenBudget tokens (as the trace counts them). Zero
// means no limit.
type Hooks struct {
	Rec         *trace.Recorder
	Step        string
	Model       model.LLM // logged by Name(), so a fallback shows the model actually used
	MaxCalls    int
	TokenBudget int

	mu    sync.Mutex
	calls int
	start time.Time
}

// Apply adds the hooks to an agent config.
func (h *Hooks) Apply(cfg *llmagent.Config) {
	cfg.BeforeModelCallbacks = append(cfg.BeforeModelCallbacks, h.beforeModel)
	cfg.AfterModelCallbacks = append(cfg.AfterModelCallbacks, h.afterModel)
	cfg.OnModelErrorCallbacks = append(cfg.OnModelErrorCallbacks, h.onModelError)
	cfg.AfterToolCallbacks = append(cfg.AfterToolCallbacks, h.afterTool)
}

// onModelError logs a model call that failed outright (after HTTP retries),
// which afterModel never sees. Returning nil, nil keeps the error.
func (h *Hooks) onModelError(ctx agent.Context, _ *model.LLMRequest, err error) (*model.LLMResponse, error) {
	h.mu.Lock()
	latency := time.Since(h.start).Milliseconds()
	h.mu.Unlock()
	e := trace.Event{Type: trace.ModelCall, Step: h.Step, Agent: ctx.AgentName(), Model: h.Model.Name(), LatencyMS: latency, Error: err.Error()}
	var empty *EmptyOutputError
	if errors.As(err, &empty) {
		e.FinishReason = empty.FinishReason
	}
	h.Rec.Emit(e)
	return nil, nil
}

// beforeModel enforces the call cap and the token budget. Returning an error
// skips the model call and ends the agent's run with that error. The budget
// is checked before a call, so the call that crosses it still completes.
func (h *Hooks) beforeModel(_ agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	if used := h.Rec.Totals().TotalTokens; h.TokenBudget > 0 && used >= h.TokenBudget {
		return nil, fmt.Errorf("%s: %w: %d of %d tokens used (AUTODOC_TOKEN_BUDGET)", h.Step, ErrTokenBudget, used, h.TokenBudget)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.MaxCalls > 0 && h.calls >= h.MaxCalls {
		return nil, fmt.Errorf("%s: model call cap of %d reached", h.Step, h.MaxCalls)
	}
	h.calls++
	h.start = time.Now()
	return nil, nil
}

// afterModel logs the call. It returns nil, nil so the response passes through unchanged.
func (h *Hooks) afterModel(ctx agent.Context, resp *model.LLMResponse, respErr error) (*model.LLMResponse, error) {
	h.mu.Lock()
	latency := time.Since(h.start).Milliseconds()
	h.mu.Unlock()

	e := trace.Event{Type: trace.ModelCall, Step: h.Step, Agent: ctx.AgentName(), Model: h.Model.Name(), LatencyMS: latency}
	if respErr != nil {
		e.Error = respErr.Error()
	}
	if resp != nil {
		e.FinishReason = string(resp.FinishReason)
		if resp.ErrorCode != "" {
			e.Error = resp.ErrorCode + ": " + resp.ErrorMessage
		}
		if u := resp.UsageMetadata; u != nil {
			e.InputTokens = int(u.PromptTokenCount + u.ToolUsePromptTokenCount)
			// Gemini and OpenAI-compatible servers count cache hits within the prompt tokens.
			e.CachedTokens = int(u.CachedContentTokenCount)
			e.OutputTokens = int(u.CandidatesTokenCount)
			e.ThoughtTokens = int(u.ThoughtsTokenCount)
			e.TotalTokens = int(u.TotalTokenCount)
			if e.TotalTokens == 0 { // some OpenAI-compatible servers leave the total out
				e.TotalTokens = e.InputTokens + e.OutputTokens + e.ThoughtTokens
			}
		}
	}
	h.Rec.Emit(e)
	return nil, nil
}

// afterTool logs the call. ADK runs it for failed calls too, with err set;
// the error then goes back to the model as the tool's response.
func (h *Hooks) afterTool(ctx agent.Context, t tool.Tool, args, result map[string]any, err error) (map[string]any, error) {
	e := trace.Event{Type: trace.ToolCall, Step: h.Step, Agent: ctx.AgentName(), Tool: t.Name(), Args: args}
	if err != nil {
		e.Error = err.Error()
	}
	if b, mErr := json.Marshal(result); mErr == nil && result != nil {
		e.ResultBytes = len(b)
	}
	h.Rec.Emit(e)
	return nil, nil
}
