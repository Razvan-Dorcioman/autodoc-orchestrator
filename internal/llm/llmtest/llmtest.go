// Package llmtest is a fake OpenAI-compatible model server for agent tests.
// It records what each request sends, so tests can check the part of it that
// providers cache.
package llmtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"autodoc/internal/llm"
)

// Usage the server reports on every reply: PromptTokens of input, of which
// CachedTokens came from its prompt cache.
const (
	PromptTokens = 100
	CachedTokens = 80
)

// Request is the start of one recorded chat completion request: its first
// message and its tool declarations, as sent.
type Request struct {
	Role   string          // of the first message
	System json.RawMessage // the first message
	Tools  json.RawMessage
}

// Server answers every /v1/chat/completions request with the text reply.
type Server struct {
	reply string
	srv   *httptest.Server

	mu   sync.Mutex
	reqs []Request
}

// Start runs a server until the test ends.
func Start(t testing.TB, reply string) *Server {
	s := &Server{reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// Provider points the local provider at the server.
func (s *Server) Provider() llm.Provider {
	return llm.Provider{Kind: llm.ProviderLocal, APIKey: "test", BaseURL: s.srv.URL + "/v1"}
}

// Requests returns the requests so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	if err := json.Unmarshal(data, &body); err != nil || len(body.Messages) == 0 {
		http.Error(w, fmt.Sprintf("bad request body: %v", err), http.StatusBadRequest)
		return
	}
	var first struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(body.Messages[0], &first)
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Role: first.Role, System: body.Messages[0], Tools: body.Tools})
	s.mu.Unlock()

	reply, _ := json.Marshal(s.reply)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":%q,
		"choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":%d,"completion_tokens":2,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
		body.Model, reply, PromptTokens, PromptTokens+2, CachedTokens)
}

// CheckStablePrefix fails t unless every request starts with the system
// prompt and has the same system prompt and tool declarations, byte for byte.
// That shared start is what provider prompt caches match on: anything per run,
// session or input in it would make every call a cache miss.
func CheckStablePrefix(t testing.TB, reqs []Request) {
	t.Helper()
	if len(reqs) < 2 {
		t.Fatalf("%d requests, want at least 2 to compare", len(reqs))
	}
	for i, r := range reqs {
		if r.Role != "system" {
			t.Errorf("request %d starts with a %q message, want the system prompt", i, r.Role)
		}
		if !bytes.Equal(r.System, reqs[0].System) {
			t.Fatalf("request %d has a different system prompt than request 0:\n%s\nwant:\n%s", i, r.System, reqs[0].System)
		}
		if !bytes.Equal(r.Tools, reqs[0].Tools) {
			t.Fatalf("request %d declares different tools than request 0:\n%s\nwant:\n%s", i, r.Tools, reqs[0].Tools)
		}
	}
}
