package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParsePrice(t *testing.T) {
	p, err := ParsePrice(" 1.25 / 10 ")
	if err != nil || p != (Price{InputPerM: 1.25, OutputPerM: 10}) {
		t.Errorf("ParsePrice = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "1.25", "a/b", "1/-2", "1/2/3"} {
		if _, err := ParsePrice(bad); err == nil {
			t.Errorf("ParsePrice(%q) accepted", bad)
		}
	}
}

func TestCostPerCallAndRun(t *testing.T) {
	rec, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec.SetPricing(Pricing{
		Models:        map[string]Price{"paid": {InputPerM: 2, OutputPerM: 8}},
		ReferenceName: "ref", Reference: Price{InputPerM: 1, OutputPerM: 10},
	})
	// paid: 1M in at $2 + (0.5M out + 0.25M thought) at $8 = $8; reference $1 + $7.5.
	rec.Emit(Event{Type: ModelCall, Model: "paid", InputTokens: 1_000_000, OutputTokens: 500_000, ThoughtTokens: 250_000})
	// free: $0; reference 1000 in at $1/M + 100 out at $10/M = $0.002.
	rec.Emit(Event{Type: ModelCall, Model: "free", InputTokens: 1000, OutputTokens: 100})
	rec.Emit(Event{Type: ModelCall, Error: "boom"}) // failed before any usage
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(filepath.Join(rec.Dir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var calls, ends []Event
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case ModelCall:
			calls = append(calls, e)
		case RunEnd:
			ends = append(ends, e)
		case RunStart:
			if e.EstCostUSD != nil {
				t.Errorf("run_start has a cost: %+v", e)
			}
		}
	}
	want := [][2]float64{{8, 8.5}, {0, 0.002}, {0, 0}}
	for i, e := range calls {
		if e.EstCostUSD == nil || e.RefCostUSD == nil {
			t.Fatalf("call %d has no cost: %+v", i, e)
		}
		if got := [2]float64{*e.EstCostUSD, *e.RefCostUSD}; got != want[i] {
			t.Errorf("call %d cost = %v, want %v", i, got, want[i])
		}
	}
	if len(ends) != 1 || ends[0].EstCostUSD == nil || *ends[0].EstCostUSD != 8 || *ends[0].RefCostUSD != 8.502 {
		t.Errorf("run_end = %+v", ends)
	}

	data, err := os.ReadFile(filepath.Join(rec.Dir(), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	c := s.Cost
	if c.USD != 8 || c.ReferenceUSD != 8.502 || c.ReferenceModel != "ref" || c.Note == "" {
		t.Errorf("cost = %+v", c)
	}
	if !slices.Equal(c.FreeModels, []string{"free"}) || !slices.Equal(c.PricedModels, []string{"paid"}) {
		t.Errorf("free %v, priced %v", c.FreeModels, c.PricedModels)
	}
	if m := s.Models["paid"]; m.EstCostUSD != 8 || m.RefCostUSD != 8.5 {
		t.Errorf("paid model totals = %+v", m)
	}
	if m := s.Models["free"]; m.EstCostUSD != 0 || m.RefCostUSD != 0.002 {
		t.Errorf("free model totals = %+v", m)
	}
}

func TestDefaultPricingCountsEverythingFree(t *testing.T) {
	rec, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	rec.Emit(Event{Type: ModelCall, Model: "qwen3:14b", InputTokens: 200_000, OutputTokens: 20_000})
	c := rec.Summary().Cost
	// Reference gemini-2.5-pro: 0.2M at $1.25 + 0.02M at $10 = $0.45.
	if c.USD != 0 || c.ReferenceUSD != 0.45 || c.ReferenceModel != "gemini-2.5-pro" {
		t.Errorf("cost = %+v", c)
	}
}
