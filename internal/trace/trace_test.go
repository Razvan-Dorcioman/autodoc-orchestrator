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
