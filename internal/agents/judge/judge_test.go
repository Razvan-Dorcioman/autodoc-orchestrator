package judge

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"autodoc/internal/gofacts"
	"autodoc/internal/llm"
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

func TestParseVerdict(t *testing.T) {
	for _, tt := range []struct {
		in, verdict, reason string
		ok                  bool
	}{
		{in: `{"verdict": "supported", "reason": "line 8 divides by 100"}`, verdict: Supported, reason: "line 8 divides by 100", ok: true},
		{in: "```json\n{\"verdict\": \"Unsupported\", \"reason\": \" no \"}\n```", verdict: Unsupported, reason: "no", ok: true},
		{in: "<think>{\"verdict\": \"supported\"}</think>\nVerdict: {\"verdict\": \"unverifiable\", \"reason\": \"elsewhere\"}", verdict: Unverifiable, reason: "elsewhere", ok: true},
		{in: `{"verdict": "partly", "reason": "x"}`},
		{in: `supported`},
		{in: `{"verdict": "supported", "reason": `},
	} {
		v, r, ok := ParseVerdict(tt.in)
		if v != tt.verdict || r != tt.reason || ok != tt.ok {
			t.Errorf("ParseVerdict(%q) = %q, %q, %v; want %q, %q, %v", tt.in, v, r, ok, tt.verdict, tt.reason, tt.ok)
		}
	}
}

func TestRank(t *testing.T) {
	claims := []verify.Claim{
		{Page: "docs/structure.md", Line: 3, Text: "The package `internal/shop` holds the store."},
		{Page: "docs/business-rules.md", Line: 9, Text: "Amounts are converted with `internal/shop/price.go:Convert`, rounded half away from zero to 2 decimals.",
			Citations: []verify.Citation{{Ref: "internal/shop/price.go:Convert"}}},
		{Page: "docs/structure.md", Line: 5, Text: "The package `internal/api` holds the handlers."},
		{Page: "docs/api.md", Line: 12, Text: "| qty | query | 1 | at most 10 |"},
	}
	got := Rank(claims)
	want := []int{1, 3, 0, 2} // risk first; ties keep page and line order
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Rank = %v, want %v", got, want)
		}
	}
}

// fakeJudge answers by the claim text in the request.
type fakeJudge struct{ calls []string }

func (f *fakeJudge) Name() string { return "fake" }

func (f *fakeJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var claim string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if strings.Contains(p.Text, "Claim (") {
					claim = p.Text
				}
			}
		}
		f.calls = append(f.calls, claim)
		answer := `{"verdict": "supported", "reason": "matches"}`
		switch {
		case strings.Contains(claim, "100 * 0.2 = 20"):
			answer = `{"verdict": "unsupported", "reason": "line 8 divides by 100 first: 100 cents is 0.2 EUR"}`
		case strings.Contains(claim, "mumble"):
			answer = "I am not sure."
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(answer, genai.RoleModel)}, nil)
	}
}

const priceGo = `package shop

var rates = map[string]float64{"EUR": 0.2}

// Convert turns an amount in cents into the target currency.
func Convert(cents int64, to string) float64 {
	return float64(cents) / 100 * rates[to]
}
`

func writeDocs(t *testing.T, dir, rules string) {
	t.Helper()
	for _, p := range verify.DocPages {
		content := "# Page\n"
		if p == "docs/business-rules.md" {
			content = rules
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"docs", "internal/shop"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "internal/shop/price.go"), []byte(priceGo), 0o644); err != nil {
		t.Fatal(err)
	}
	facts := &gofacts.Facts{Packages: []gofacts.Package{{Name: "shop", Dir: "internal/shop", Files: []string{"internal/shop/price.go"}}}}
	rules := "# Business rules\n\n" +
		"- `internal/shop/price.go:Convert` converts an amount in cents to another currency.\n" + // 3
		"  - Example: `100 cents → 100 * 0.2 = 20 EUR`, rounded to the cent.\n" + // 4
		"  - The rate table holds mumble for each currency.\n" + // 5
		"No citation on this line, so nothing to check.\n" // 6
	writeDocs(t, dir, rules)

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fake := &fakeJudge{}
	fb, err := llm.NewFallback([]model.LLM{fake}, rec)
	if err != nil {
		t.Fatal(err)
	}
	j := &Judge{rec: rec, model: fb, verdicts: map[string]Result{}, budget: 2}

	rep, err := j.Check(context.Background(), dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	// Line 4 has numbers and arithmetic, so it goes first; line 3 names a
	// unit, so it is next; line 5 is over the budget.
	verdicts := map[int]string{}
	for _, r := range rep.Results {
		verdicts[r.Line] = r.Verdict
	}
	if want := map[int]string{3: Supported, 4: Unsupported, 5: Skipped}; !equal(verdicts, want) {
		t.Errorf("verdicts = %v, want %v", verdicts, want)
	}
	if rep.Claims != 3 || rep.Judged != 2 || rep.OK() || len(fake.calls) != 2 {
		t.Errorf("claims %d judged %d ok %v model calls %d; want 3, 2, false, 2", rep.Claims, rep.Judged, rep.OK(), len(fake.calls))
	}
	if got := rep.Uncited["docs/business-rules.md"]; len(got) != 1 || got[0] != 6 {
		t.Errorf("uncited = %v, want line 6", rep.Uncited)
	}
	issues := rep.Issues()
	if len(issues) != 1 || issues[0].Check != "judge" || issues[0].Where != "docs/business-rules.md:4" ||
		!strings.Contains(issues[0].Message, "divides by 100 first") || !strings.Contains(issues[0].Message, "`internal/shop/price.go:Convert`") {
		t.Errorf("issues = %+v", issues)
	}

	// After a fix only the changed line and the skipped one are judged; the
	// skipped one gets no parseable verdict, even when asked again.
	writeDocs(t, dir, strings.Replace(rules, "100 * 0.2 = 20 EUR", "100 / 100 * 0.2 = 0.2 EUR", 1))
	fake.calls = nil
	rep, err = j.Check(context.Background(), dir, facts)
	if err != nil {
		t.Fatal(err)
	}
	cached := 0
	verdicts = map[int]string{}
	for _, r := range rep.Results {
		verdicts[r.Line] = r.Verdict
		if r.Cached {
			cached++
		}
	}
	if want := map[int]string{3: Supported, 4: Supported, 5: Unverifiable}; !equal(verdicts, want) {
		t.Errorf("second pass: verdicts = %v, want %v", verdicts, want)
	}
	if rep.Judged != 2 || cached != 1 || !rep.OK() || len(fake.calls) != 3 {
		t.Errorf("second pass: judged %d cached %d ok %v model calls %d; want 2, 1, true, 3", rep.Judged, cached, rep.OK(), len(fake.calls))
	}
}

func equal(a, b map[int]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestMessage(t *testing.T) {
	msg := Message(verify.Claim{
		Page: "docs/api.md", Line: 7, Text: "| qty | 1 |", Context: "## GET /orders\n| Name | Default |", Inherited: true,
		Citations: []verify.Citation{{Ref: "GET /orders", File: "internal/shop/handlers.go", Snippet: "  12  func get() {}"}},
	})
	code := "Cited code:\n`GET /orders` (internal/shop/handlers.go):\n```go\n  12  func get() {}\n```\n"
	if !strings.HasPrefix(msg, code) {
		t.Errorf("message does not start with the cited code:\n%s", msg)
	}
	for _, want := range []string{"| Name | Default |", "cites nothing itself", "Claim (docs/api.md, line 7):\n| qty | 1 |\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if !strings.HasSuffix(msg, "| qty | 1 |\n") {
		t.Errorf("message does not end with the claim:\n%s", msg)
	}
}

const feeGo = `package shop

// Fee is the handling fee in cents, capped at 500.
func Fee(cents int64) int64 {
	return min(cents/10, 500)
}
`

// TestCheckJudgesSameCodeBackToBack: risk picks the claims that fit the
// budget, and the claims citing the same code are judged one after the other.
func TestCheckJudgesSameCodeBackToBack(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"docs", "internal/shop"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, src := range map[string]string{"price.go": priceGo, "fee.go": feeGo} {
		if err := os.WriteFile(filepath.Join(dir, "internal/shop", name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	facts := &gofacts.Facts{Packages: []gofacts.Package{{Name: "shop", Dir: "internal/shop",
		Files: []string{"internal/shop/fee.go", "internal/shop/price.go"}}}}
	writeDocs(t, dir, "# Business rules\n\n"+
		"- `internal/shop/price.go:Convert` divides by 100 and never rounds.\n"+ // 3: riskiest
		"- `internal/shop/fee.go:Fee` caps the fee at 500 cents.\n"+ // 4
		"- `internal/shop/price.go:Convert` returns the result as a float64.\n"+ // 5
		"- `internal/shop/fee.go:Fee` is part of the package API.\n") // 6: least risky, over the budget

	rec, err := trace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fake := &fakeJudge{}
	fb, err := llm.NewFallback([]model.LLM{fake}, rec)
	if err != nil {
		t.Fatal(err)
	}
	j := &Judge{rec: rec, model: fb, verdicts: map[string]Result{}, budget: 3}
	rep, err := j.Check(context.Background(), dir, facts)
	if err != nil {
		t.Fatal(err)
	}

	lineRe := regexp.MustCompile(`Claim \(docs/business-rules\.md, line (\d+)\)`)
	var order []string
	for _, call := range fake.calls {
		m := lineRe.FindStringSubmatch(call)
		if m == nil {
			t.Fatalf("no claim line in the message:\n%s", call)
		}
		order = append(order, m[1])
	}
	// fee.go sorts before price.go; within a file, riskiest first.
	if want := []string{"4", "3", "5"}; !slices.Equal(order, want) {
		t.Errorf("judged lines %v, want %v", order, want)
	}
	for _, r := range rep.Results {
		if skipped := r.Verdict == Skipped; skipped != (r.Line == 6) {
			t.Errorf("line %d: verdict %s", r.Line, r.Verdict)
		}
	}
}
