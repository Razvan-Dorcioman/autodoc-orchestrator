package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/genai"

	"autodoc/internal/trace"
)

// emptyChain serves names from a fakeOllama with the given empty replies and
// skips the backoff waits.
func emptyChain(t *testing.T, empty map[string]int, names ...string) (*Fallback, *fakeOllama, *trace.Recorder) {
	t.Helper()
	old := testSleep
	testSleep = func(time.Duration) {}
	t.Cleanup(func() { testSleep = old })

	fake := &fakeOllama{empty: empty}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	fb, err := NewModelChain(context.Background(), names, Provider{Kind: ProviderLocal, APIKey: "local", BaseURL: srv.URL + "/v1"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	return fb, fake, rec
}

func generateText(fb *Fallback) (string, error) {
	req := &model.LLMRequest{
		Model:    fb.Name(),
		Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)},
	}
	var text string
	for resp, err := range fb.GenerateContent(context.Background(), req, false) {
		if err != nil {
			return "", err
		}
		text += resp.Content.Parts[0].Text
	}
	return text, nil
}

// traceEvents reads back the events of type typ that rec has written.
func traceEvents(t *testing.T, rec *trace.Recorder, typ string) []trace.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(rec.Dir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []trace.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e trace.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestEmptyReplyIsRetried(t *testing.T) {
	fb, fake, rec := emptyChain(t, map[string]int{"qwen3:14b": 1}, "qwen3:14b")

	text, err := generateText(fb)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if text != "from qwen3:14b" || len(fake.asked) != 2 {
		t.Errorf("text = %q after %d requests, want the answer on the second", text, len(fake.asked))
	}
	retries := traceEvents(t, rec, trace.Retry)
	if len(retries) != 1 || retries[0].FinishReason != "length" {
		t.Errorf("retry events = %+v, want one with finish_reason length", retries)
	}
}

func TestEmptyRepliesFallBack(t *testing.T) {
	fb, fake, rec := emptyChain(t, map[string]int{"qwen3:14b": -1}, "qwen3:14b", "small:8b")

	text, err := generateText(fb)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if text != "from small:8b" || fb.Name() != "small:8b" {
		t.Errorf("text = %q, active = %s; want the answer from small:8b", text, fb.Name())
	}
	if n := len(fake.asked); n != 4 {
		t.Errorf("%d requests, want 3 empty attempts and 1 on the fallback", n)
	}
	if got := rec.Totals().Fallbacks; got != 1 {
		t.Errorf("fallbacks = %d, want 1", got)
	}
}

func TestEmptyReplyErrorCarriesFinishReason(t *testing.T) {
	fb, _, _ := emptyChain(t, map[string]int{"qwen3:14b": -1}, "qwen3:14b")

	_, err := generateText(fb)
	var empty *EmptyOutputError
	if !errors.As(err, &empty) {
		t.Fatalf("err = %v, want an EmptyOutputError", err)
	}
	if empty.FinishReason != "length" || empty.ReasoningChars == 0 || !errors.Is(err, openaimodel.ErrNoTextOrToolContent) {
		t.Errorf("err = %+v, want finish_reason length, the reasoning length and ADK's error underneath", empty)
	}
}
