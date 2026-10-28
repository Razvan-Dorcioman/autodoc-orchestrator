package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderWritesTraceAndSummary(t *testing.T) {
	rec, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec.Emit(Event{Type: ModelCall, Step: "s", InputTokens: 10, OutputTokens: 4, TotalTokens: 14})
	rec.Emit(Event{Type: ToolCall, Step: "s", Tool: "read_file"})
	rec.Emit(Event{Type: ToolCall, Step: "s", Tool: "read_file", Error: "outside the repository"})
	rec.Emit(Event{Type: Retry, Step: "s"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(filepath.Join(rec.Dir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var types []string
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		if e.Seq != len(types)+1 {
			t.Errorf("seq = %d, want %d", e.Seq, len(types)+1)
		}
		types = append(types, e.Type)
	}
	want := []string{RunStart, ModelCall, ToolCall, ToolCall, Retry, RunEnd}
	if len(types) != len(want) {
		t.Fatalf("types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("types[%d] = %s, want %s", i, types[i], want[i])
		}
	}

	data, err := os.ReadFile(filepath.Join(rec.Dir(), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Totals
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.ModelCalls != 1 || s.ToolCalls != 2 || s.ToolErrors != 1 || s.Retries != 1 || s.TotalTokens != 14 || s.RunID != rec.RunID() {
		t.Errorf("summary = %+v", s)
	}
}

func TestSummaryStepsModelsAndOutcome(t *testing.T) {
	rec, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec.Emit(Event{Type: StepStart, Step: "docs"})
	rec.Emit(Event{Type: ModelCall, Step: "docs", Model: "m1", TotalTokens: 10})
	rec.Emit(Event{Type: ModelCall, Step: "judge", Model: "m2", TotalTokens: 5}) // inside the docs step
	rec.Emit(Event{Type: StepEnd, Step: "docs"})
	rec.Emit(Event{Type: StepStart, Step: "commit"})
	rec.Emit(Event{Type: StepEnd, Step: "commit", Error: "boom"})
	if s := rec.Summary(); s.Status != Incomplete || s.FailedStep != "" {
		t.Errorf("before Finish: %+v", s)
	}
	rec.Finish(Failed, map[string]string{"branch": "autodoc/abc"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(rec.Dir(), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Status != Failed || s.FailedStep != "commit" || s.Error != "boom" || s.ModelCalls != 2 {
		t.Errorf("summary = %+v", s)
	}
	if len(s.Steps) != 2 || s.Steps[0].ModelCalls != 2 || s.Steps[0].TotalTokens != 15 || s.Steps[1].Error != "boom" {
		t.Errorf("steps = %+v", s.Steps)
	}
	if s.Models["m1"].Calls != 1 || s.Models["m2"].TotalTokens != 5 {
		t.Errorf("models = %+v", s.Models)
	}
	if r, _ := s.Result.(map[string]any); r["branch"] != "autodoc/abc" {
		t.Errorf("result = %v", s.Result)
	}
}
