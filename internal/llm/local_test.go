package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"autodoc/internal/trace"
)

// fakeOllama answers /v1/chat/completions like an OpenAI-compatible local
// server: 404 for models in missing, a reasoning-only reply with no answer for
// the next empty[model] requests (every request if negative), and a one-word
// completion otherwise.
type fakeOllama struct {
	missing map[string]bool
	empty   map[string]int

	mu    sync.Mutex
	paths []string
	auths []string
	asked []string // model of each request
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 1<<16)
	n, _ := r.Body.Read(body)
	name := ""
	if i := strings.Index(string(body[:n]), `"model":"`); i >= 0 {
		rest := string(body[:n])[i+len(`"model":"`):]
		name = rest[:strings.Index(rest, `"`)]
	}
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.asked = append(f.asked, name)
	empty := f.empty[name] != 0
	if f.empty[name] > 0 {
		f.empty[name]--
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if empty {
		fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":%q,
			"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"hmm, let me think"},"finish_reason":"length"}],
			"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`, name)
		return
	}
	if f.missing[name] {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"message":"model %q not found, try pulling it first","type":"api_error","param":null,"code":null}}`, name)
		return
	}
	fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":%q,
		"choices":[{"index":0,"message":{"role":"assistant","content":"from %s"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, name, name)
}

func TestLocalProviderFallsBackWhenModelIsNotPulled(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
	fake := &fakeOllama{missing: map[string]bool{"big:70b": true}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	p := Provider{Kind: ProviderLocal, APIKey: "local", BaseURL: srv.URL + "/v1"}
	fb, err := NewModelChain(context.Background(), []string{"big:70b", "small:8b"}, p, rec)
	if err != nil {
		t.Fatal(err)
	}

	req := &model.LLMRequest{
		Model:    fb.Name(),
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
	}
	var got *model.LLMResponse
	for resp, err := range fb.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		got = resp
	}
	if got == nil || got.Content.Parts[0].Text != "from small:8b" {
		t.Fatalf("response = %+v, want text from small:8b", got)
	}
	if fb.Name() != "small:8b" || rec.Totals().Fallbacks != 1 {
		t.Errorf("Name = %s, fallbacks = %d; want small:8b and 1", fb.Name(), rec.Totals().Fallbacks)
	}
	for _, path := range fake.paths {
		if path != "/v1/chat/completions" {
			t.Errorf("request path %q, want /v1/chat/completions", path)
		}
	}
	for _, auth := range fake.auths {
		if auth != "Bearer local" {
			t.Errorf("Authorization = %q, want the placeholder key, never OPENAI_API_KEY", auth)
		}
	}
	if got.UsageMetadata == nil || got.UsageMetadata.TotalTokenCount != 5 {
		t.Errorf("usage = %+v, want 5 total tokens", got.UsageMetadata)
	}
}

func TestNewModelRejectsUnknownProvider(t *testing.T) {
	if _, err := NewModel(context.Background(), "m", Provider{Kind: "nope"}, nil); err == nil {
		t.Error("want an error for an unknown provider")
	}
}
