// Package trace records a run as JSON lines: steps, tool calls, model calls,
// retries and gate results, with token counts. Each run gets its own
// directory holding trace.jsonl and, on Close, summary.json.
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// Run statuses in summary.json.
const (
	Committed  = "committed"  // the deliverables are committed on the run branch (and pushed, if asked)
	Unchanged  = "unchanged"  // every gate passed and the working copy equals the base commit
	Failed     = "failed"     // a step failed; the working copy was restored and nothing is committed
	Incomplete = "incomplete" // the run ended without saying how, e.g. interrupted
)

// Summary is summary.json: the totals, each step with its outcome and its
// model calls, the model calls per model, and how the run ended.
type Summary struct {
	Totals
	Status     string                 `json:"status"`
	FailedStep string                 `json:"failed_step,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Steps      []StepSummary          `json:"steps"`
	Models     map[string]ModelTotals `json:"models"`
	Result     any                    `json:"result,omitempty"` // what the run left behind, from the pipeline
}

// StepSummary is one step of the pipeline. Model calls count toward the step
// that was running, so the judge's calls are the docs step's.
type StepSummary struct {
	Name        string `json:"name"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
	ModelCalls  int    `json:"model_calls"`
	TotalTokens int    `json:"total_tokens"`
}

type ModelTotals struct {
	Calls        int `json:"calls"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Totals counts the whole run.
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

	steps     []StepSummary
	open      int // index in steps of the running step, or -1
	opened    time.Time
	models    map[string]ModelTotals
	status    string
	result    any
	failed    string // the step that failed and its error
	failedErr string
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
	r := &Recorder{dir: dir, f: f, enc: json.NewEncoder(f), totals: Totals{RunID: id},
		open: -1, models: map[string]ModelTotals{}, status: Incomplete}
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
		m := r.models[e.Model]
		m.Calls++
		m.InputTokens += e.InputTokens
		m.OutputTokens += e.OutputTokens
		m.TotalTokens += e.TotalTokens
		r.models[e.Model] = m
		if r.open >= 0 {
			r.steps[r.open].ModelCalls++
			r.steps[r.open].TotalTokens += e.TotalTokens
		}
	case StepStart:
		r.steps = append(r.steps, StepSummary{Name: e.Step})
		r.open, r.opened = len(r.steps)-1, e.Time
	case StepEnd:
		if r.open >= 0 && r.steps[r.open].Name == e.Step {
			r.steps[r.open].DurationMS = e.Time.Sub(r.opened).Milliseconds()
			r.steps[r.open].Error = e.Error
			r.open = -1
		}
		if e.Error != "" {
			r.failed, r.failedErr = e.Step, e.Error
		}
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

// Finish records how the run ended, for summary.json: one of the run
// statuses, and what the run left behind (JSON-encodable).
func (r *Recorder) Finish(status string, result any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.result = status, result
}

// Summary returns summary.json as it stands. A failed run names the last
// step that failed and its error.
func (r *Recorder) Summary() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Summary{Totals: r.totals, Status: r.status, Steps: slices.Clone(r.steps), Models: maps.Clone(r.models), Result: r.result}
	if s.Status == Failed {
		s.FailedStep, s.Error = r.failed, r.failedErr
	}
	return s
}

// Close writes run_end and summary.json and closes the trace file.
func (r *Recorder) Close() error {
	r.Emit(Event{Type: RunEnd, Detail: r.Summary().Status})
	summary, err := json.MarshalIndent(r.Summary(), "", "  ")
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
