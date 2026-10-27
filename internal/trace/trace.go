// Package trace records a run as JSON lines: steps, tool calls, model calls,
// retries and gate results, with token counts. Each run gets its own
// directory holding trace.jsonl and, on Close, summary.json.
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event types.
const (
	RunStart  = "run_start"
	RunEnd    = "run_end"
	StepStart = "step_start"
	StepEnd   = "step_end"
	ModelCall = "model_call"
	ToolCall  = "tool_call"
	Turn      = "agent_turn" // one user message to an agent and its final reply
	Retry     = "retry"
	Fallback  = "fallback"
	Gate      = "gate"
)

// Event is one line of trace.jsonl. Fields that do not apply to a type are omitted.
type Event struct {
	Time  time.Time `json:"ts"`
	Seq   int       `json:"seq"`
	Type  string    `json:"type"`
	Step  string    `json:"step,omitempty"`
	Agent string    `json:"agent,omitempty"`

	// Model calls.
	Model         string `json:"model,omitempty"`
	InputTokens   int    `json:"input_tokens,omitempty"`
	OutputTokens  int    `json:"output_tokens,omitempty"`
	ThoughtTokens int    `json:"thought_tokens,omitempty"`
	TotalTokens   int    `json:"total_tokens,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`

	// Tool calls.
	Tool        string         `json:"tool,omitempty"`
	Args        map[string]any `json:"args,omitempty"`
	ResultBytes int            `json:"result_bytes,omitempty"`

	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Totals is what summary.json reports.
type Totals struct {
	RunID         string `json:"run_id"`
	ModelCalls    int    `json:"model_calls"`
	ToolCalls     int    `json:"tool_calls"`
	ToolErrors    int    `json:"tool_errors"`
	Retries       int    `json:"retries"`
	Fallbacks     int    `json:"fallbacks"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	ThoughtTokens int    `json:"thought_tokens"`
	TotalTokens   int    `json:"total_tokens"`
}

// Recorder appends events to a run's trace.jsonl. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	dir    string
	f      *os.File
	enc    *json.Encoder
	seq    int
	totals Totals
}

// New creates runsDir/<run-id>/ and opens its trace.jsonl.
func New(runsDir string) (*Recorder, error) {
	id, err := newRunID(time.Now())
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.Create(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}
	r := &Recorder{dir: dir, f: f, enc: json.NewEncoder(f), totals: Totals{RunID: id}}
	r.Emit(Event{Type: RunStart})
	return r, nil
}

func (r *Recorder) RunID() string { return r.totals.RunID }
func (r *Recorder) Dir() string   { return r.dir }

// Emit stamps e with the time and a sequence number and appends it.
// A failed write is not fatal to the run; it is reported once on Close.
func (r *Recorder) Emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	r.count(e)
	_ = r.enc.Encode(e)
}

func (r *Recorder) count(e Event) {
	t := &r.totals
	switch e.Type {
	case ModelCall:
		t.ModelCalls++
		t.InputTokens += e.InputTokens
		t.OutputTokens += e.OutputTokens
		t.ThoughtTokens += e.ThoughtTokens
		t.TotalTokens += e.TotalTokens
	case ToolCall:
		t.ToolCalls++
		if e.Error != "" {
			t.ToolErrors++
		}
	case Retry:
		t.Retries++
	case Fallback:
		t.Fallbacks++
	}
}

// Totals returns the counts so far.
func (r *Recorder) Totals() Totals {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totals
}

// Close writes run_end and summary.json and closes the trace file.
func (r *Recorder) Close() error {
	r.Emit(Event{Type: RunEnd})
	summary, err := json.MarshalIndent(r.Totals(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.dir, "summary.json"), append(summary, '\n'), 0o644); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return r.f.Close()
}

// newRunID is sortable by time and unique enough for concurrent runs.
func newRunID(now time.Time) (string, error) {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}
