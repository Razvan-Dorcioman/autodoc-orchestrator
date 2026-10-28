package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

// An OpenAI-compatible server can answer 200 with a reply that holds no text
// and no tool call: a thinking model (qwen3 on Ollama) that spent its turn
// reasoning, or ran out of output tokens before the answer. ADK rejects such a
// reply after the HTTP layer has called it a success. The retry transport
// spots it in the body and retries it like an overload; when the retries run
// out, emptyOutputModel hands the error on with the finish reason attached so
// the fallback chain and the trace can see it.

// EmptyOutputError is a model reply with nothing in it.
type EmptyOutputError struct {
	FinishReason   string // as the server gave it; "" if it gave none
	ReasoningChars int    // length of the reasoning the server returned beside the empty answer
	Err            error
}

func (e *EmptyOutputError) Error() string {
	return fmt.Sprintf("%v (%s)", e.Err, describeEmpty(e.FinishReason, e.ReasoningChars))
}

func (e *EmptyOutputError) Unwrap() error { return e.Err }

func describeEmpty(finishReason string, reasoningChars int) string {
	if finishReason == "" {
		finishReason = "none given"
	}
	s := "finish_reason " + finishReason
	if reasoningChars > 0 {
		s += fmt.Sprintf(", %d chars of reasoning and no answer", reasoningChars)
	}
	return s
}

// isEmptyOutput reports whether err is ADK's error for a reply with nothing to convert.
func isEmptyOutput(err error) bool {
	return errors.Is(err, openaimodel.ErrNoTextOrToolContent) || errors.Is(err, openaimodel.ErrNoChoices)
}

// emptySeen carries what the transport saw in an empty reply back to the
// model call that made the request, through the request context.
type emptySeen struct {
	mu             sync.Mutex
	finishReason   string
	reasoningChars int
}

type emptySeenKey struct{}

// emptyOutputModel wraps a model served over Chat Completions so an
// empty-output error carries what the transport saw.
type emptyOutputModel struct {
	model.LLM
}

func (m emptyOutputModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		seen := &emptySeen{}
		ctx := context.WithValue(ctx, emptySeenKey{}, seen)
		for resp, err := range m.LLM.GenerateContent(ctx, req, stream) {
			if err != nil && isEmptyOutput(err) {
				seen.mu.Lock()
				err = &EmptyOutputError{FinishReason: seen.finishReason, ReasoningChars: seen.reasoningChars, Err: err}
				seen.mu.Unlock()
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

// emptyCompletion reports whether resp is a non-streamed Chat Completions
// reply with no text, refusal or tool call, the same test ADK applies. It
// reads the body and puts back a copy, and notes what it found in the
// request's emptySeen, if any.
func emptyCompletion(req *http.Request, resp *http.Response) (finishReason string, reasoningChars int, empty bool) {
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(req.URL.Path, "/chat/completions") ||
		!strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return "", 0, false
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return "", 0, false // let the caller report the short body
	}

	var c struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string            `json:"content"`
				Refusal          string            `json:"refusal"`
				Reasoning        string            `json:"reasoning"`         // Ollama
				ReasoningContent string            `json:"reasoning_content"` // vLLM, llama.cpp
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &c) != nil {
		return "", 0, false // not ours to judge; ADK reports a body it cannot parse
	}
	if len(c.Choices) > 0 {
		ch := c.Choices[0]
		msg := ch.Message
		if msg.Content != "" || msg.Refusal != "" || len(msg.ToolCalls) > 0 {
			return "", 0, false
		}
		finishReason, reasoningChars = ch.FinishReason, len(msg.Reasoning)+len(msg.ReasoningContent)
	}
	if seen, ok := req.Context().Value(emptySeenKey{}).(*emptySeen); ok {
		seen.mu.Lock()
		seen.finishReason, seen.reasoningChars = finishReason, reasoningChars
		seen.mu.Unlock()
	}
	return finishReason, reasoningChars, true
}
