// Package judge is the LLM step that fact-checks the docs.
//
// The pages are split into claims deterministically (verify.SplitClaims):
// one line plus the code it cites. Each claim is one model call in a fresh
// session, which answers supported, unsupported or unverifiable with a
// reason. Unsupported claims fail the pass; the pipeline sends them back to
// the docs agent session that wrote the page.
package judge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/llm"
	"autodoc/internal/trace"
	"autodoc/internal/verify"
)

// Step is the trace step name of the judge's calls.
const Step = "judge"

// MaxClaims is how many claims one pass judges. A warm local 14B model takes
// about five seconds per claim, up to half a minute, so a pass takes 5 to
// 30 minutes on a 16 GB GPU; the riskiest claims go first and the rest are
// reported as skipped.
const MaxClaims = 60

// Verdicts. Only unsupported fails a pass: unverifiable means the cited code
// does not decide the claim, and skipped claims were over the budget.
const (
	Supported    = "supported"
	Unsupported  = "unsupported"
	Unverifiable = "unverifiable"
	Skipped      = "skipped"
)

const instruction = `You fact-check the documentation of a Go service against its code. Each message holds one claim, a
line of a docs page, with the code it cites, line-numbered. Judge only that line: the context around it is
there to make it readable and is not under review.

Verdicts:
- supported: the code shows the line is true. Paraphrase, summary and leaving out detail are fine; a table row
  is supported when every cell agrees with the code.
- unsupported: the code shows that something the line states is false: a wrong number, name, type, unit,
  default, bound, order, condition or result.
- unverifiable: the code shown neither confirms nor contradicts the line, because the behaviour is decided
  elsewhere. Prefer this to guessing; never call a line unsupported only because the code shown is incomplete.

These are easy to get subtly wrong; check each that applies:
- units, and integer vs. decimal arithmetic (amounts in minor units such as cents; integer division truncates);
- rounding: direction (down, up, half away from zero, half to even) and precision;
- order of operations: recompute every formula and worked example from the code, step by step;
- defaults and caps: the value used when an input is absent; minimums and maximums, inclusive or exclusive;
- fields that appear only under a condition (omitempty, a pointer set in one branch): a line saying a field is
  always present, or that no field is conditional, is wrong when one is;
- validation order: which error wins when several apply;
- case and whitespace handling of inputs;
- deliberate omissions, such as data that is not logged or not returned.

Comments state intent; the code decides. Reply with only a JSON object:
{"verdict": "supported" | "unsupported" | "unverifiable", "reason": "<one or two sentences>"}
For unsupported, the reason says what the code does instead, citing its line number, so the writer can fix the line.`

// Result is the verdict on one claim.
type Result struct {
	verify.Claim
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Cached  bool   `json:"cached,omitempty"` // judged in an earlier pass, with the same line and code
}

// Report is one judge pass, written to judge.json.
type Report struct {
	Claims     int              `json:"claims"`
	Judged     int              `json:"judged"` // claims sent to the model in this pass
	Budget     int              `json:"budget"` // claims a pass may send
	Verdicts   map[string]int   `json:"verdicts"`
	Uncited    map[string][]int `json:"uncited_lines,omitempty"` // prose lines with no code to check against, per page
	DurationMS int64            `json:"duration_ms"`
	Advisory   bool             `json:"advisory,omitempty"` // the last pass: its unsupported claims did not fail the run
	Results    []Result         `json:"results"`
}

// OK reports whether no claim was found unsupported.
func (r Report) OK() bool { return r.Verdicts[Unsupported] == 0 }

// Unsupported returns the claims found unsupported, in page order.
func (r Report) Unsupported() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Verdict == Unsupported {
			out = append(out, res)
		}
	}
	return out
}

// Summary is one line for the trace and the step's end.
func (r Report) Summary() string {
	return fmt.Sprintf("%d claims, %d judged now: %d supported, %d unsupported, %d unverifiable, %d skipped over the budget of %d",
		r.Claims, r.Judged, r.Verdicts[Supported], r.Verdicts[Unsupported], r.Verdicts[Unverifiable], r.Verdicts[Skipped], r.Budget)
}

// Issues are the unsupported claims, as issues for the page that holds each.
func (r Report) Issues() []verify.Issue {
	var out []verify.Issue
	for _, res := range r.Results {
		if res.Verdict != Unsupported {
			continue
		}
		var refs []string
		for _, c := range res.Citations {
			refs = append(refs, "`"+c.Ref+"`")
		}
		out = append(out, verify.Issue{
			Check: "judge",
			Where: fmt.Sprintf("%s:%d", res.Page, res.Line),
			Message: fmt.Sprintf("the fact-check found this line contradicted by the code it cites (%s): %s\n%s",
				strings.Join(refs, ", "), res.Reason, res.Text),
		})
	}
	return out
}

// Judge fact-checks the docs of one working copy. It remembers its verdicts,
// so a pass after a fix judges only the lines and code that changed.
type Judge struct {
	rec      *trace.Recorder
	model    *llm.Fallback
	verdicts map[string]Result
	sessions int
	budget   int // MaxClaims, lowered in tests

	tokenBudget int // the run's, shared with the other agents
}

// New builds the judge on cfg.JudgeModels.
func New(ctx context.Context, cfg config.Config, rec *trace.Recorder) (*Judge, error) {
	m, err := llm.NewModelChain(ctx, cfg.JudgeModels, cfg.Provider, rec)
	if err != nil {
		return nil, err
	}
	return &Judge{rec: rec, model: m, verdicts: map[string]Result{}, budget: MaxClaims, tokenBudget: cfg.TokenBudget}, nil
}

// Check splits the pages under dir into claims and judges at most MaxClaims
// of them that were not judged before, riskiest first.
func (j *Judge) Check(ctx context.Context, dir string, facts *gofacts.Facts) (Report, error) {
	start := time.Now()
	rep := Report{Budget: j.budget, Verdicts: map[string]int{}, Uncited: map[string][]int{}}
	pages, err := verify.SplitClaims(dir, facts)
	if err != nil {
		return rep, err
	}
	var claims []verify.Claim
	for _, p := range verify.DocPages {
		claims = append(claims, pages[p].Claims...)
		if u := pages[p].Uncited; len(u) > 0 {
			rep.Uncited[p] = u
		}
	}
	r, err := j.runner()
	if err != nil {
		return rep, err
	}
	rep.Claims = len(claims)
	rep.Results = make([]Result, len(claims))
	for _, i := range Rank(claims) {
		c := claims[i]
		key := cacheKey(c)
		res, ok := j.verdicts[key]
		switch {
		case ok:
			res.Claim, res.Cached = c, true
		case rep.Judged >= j.budget:
			res = Result{Claim: c, Verdict: Skipped, Reason: fmt.Sprintf("over the budget of %d claims per pass", j.budget)}
		default:
			rep.Judged++
			verdict, reason, err := j.judge(ctx, r, c)
			if err != nil {
				return rep, fmt.Errorf("judge %s:%d: %w", c.Page, c.Line, err)
			}
			res = Result{Claim: c, Verdict: verdict, Reason: reason}
			j.verdicts[key] = res
		}
		rep.Results[i] = res
		rep.Verdicts[res.Verdict]++
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	return rep, nil
}

// runner builds the agent for one pass. Its call cap leaves room for one
// re-ask per claim; the claim budget is what normally stops a pass.
func (j *Judge) runner() (*runner.Runner, error) {
	agentCfg := llmagent.Config{
		Name:        "judge",
		Description: "Fact-checks one documentation claim against the code it cites.",
		Model:       j.model,
		// A provider, not Instruction: ADK would read the JSON braces as session-state placeholders.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) { return instruction, nil },
	}
	hooks := &llm.Hooks{Rec: j.rec, Step: Step, Model: j.model, MaxCalls: 2 * j.budget, TokenBudget: j.tokenBudget}
	hooks.Apply(&agentCfg)
	a, err := llmagent.New(agentCfg)
	if err != nil {
		return nil, fmt.Errorf("build agent: %w", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "autodoc",
		Agent:             a,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("build runner: %w", err)
	}
	return r, nil
}

// judge asks for the verdict on one claim in a fresh session, and once more
// if the answer does not parse.
func (j *Judge) judge(ctx context.Context, r *runner.Runner, c verify.Claim) (verdict, reason string, err error) {
	j.sessions++
	id := fmt.Sprintf("%s-%s-%d", j.rec.RunID(), Step, j.sessions)
	msg := Message(c)
	var answer string
	for attempt := 0; attempt < 2; attempt++ {
		answer, err = j.turn(ctx, r, id, c, msg)
		if err != nil {
			return "", "", err
		}
		if v, why, ok := ParseVerdict(answer); ok {
			return v, why, nil
		}
		msg = `Reply with only the JSON object: {"verdict": "supported" | "unsupported" | "unverifiable", "reason": "..."}`
	}
	if len(answer) > 300 {
		answer = answer[:300] + "…"
	}
	return Unverifiable, "the judge gave no verdict: " + answer, nil
}

func (j *Judge) turn(ctx context.Context, r *runner.Runner, id string, c verify.Claim, msg string) (string, error) {
	content := genai.NewContentFromText(msg, genai.RoleUser)
	var answer string
	for ev, err := range r.Run(ctx, "autodoc", id, content, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			return "", fmt.Errorf("agent run: %w", err)
		}
		if ev == nil || ev.Content == nil || !ev.IsFinalResponse() {
			continue
		}
		for _, p := range ev.Content.Parts {
			answer += p.Text
		}
	}
	answer = strings.TrimSpace(answer)
	j.rec.Emit(trace.Event{Type: trace.Turn, Step: Step, Agent: "judge",
		Args: map[string]any{"session": id, "claim": fmt.Sprintf("%s:%d", c.Page, c.Line), "message_bytes": len(msg)}, Detail: answer})
	return answer, nil
}

// Message is the request for one claim: the line, where it sits on the
// page, and the code it cites.
func Message(c verify.Claim) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Claim (%s, line %d):\n%s\n\n", c.Page, c.Line, c.Text)
	if c.Context != "" {
		fmt.Fprintf(&b, "Where it sits on the page (not under review):\n%s\n\n", c.Context)
	}
	if c.Inherited {
		b.WriteString("The line cites nothing itself; this is the code cited by the list item or section it belongs to.\n")
	}
	b.WriteString("Cited code:\n")
	for _, ct := range c.Citations {
		lang := "go"
		if !strings.HasSuffix(ct.File, ".go") {
			lang = ""
		}
		fmt.Fprintf(&b, "`%s` (%s):\n```%s\n%s\n```\n", ct.Ref, ct.File, lang, ct.Snippet)
	}
	return b.String()
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// ParseVerdict reads the JSON object in a reply, allowing text or a code
// fence around it.
func ParseVerdict(s string) (verdict, reason string, ok bool) {
	s = thinkRe.ReplaceAllString(s, "")
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return "", "", false
	}
	var v struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal([]byte(s[start:end+1]), &v) != nil {
		return "", "", false
	}
	switch verdict = strings.ToLower(strings.TrimSpace(v.Verdict)); verdict {
	case Supported, Unsupported, Unverifiable:
		return verdict, strings.TrimSpace(v.Reason), true
	}
	return "", "", false
}

func cacheKey(c verify.Claim) string {
	h := sha256.New()
	for _, s := range []string{c.Page, c.Text, c.Context} {
		h.Write([]byte(s + "\x00"))
	}
	for _, ct := range c.Citations {
		h.Write([]byte(ct.Ref + "\x00" + ct.Snippet + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// riskRe matches the words of the checklist: where docs tend to go wrong.
var riskRe = regexp.MustCompile(`(?i)\b(defaults?|cap(?:ped|s)?|max\w*|min\w*|limit\w*|bounds?|round\w*|floor|ceil\w*|truncat\w*|integer|decimal|float\w*|divid\w*|divisi\w*|multipl\w*|units?|cents?|minor|percent\w*|omit\w*|only|unless|when|if|condition\w*|optional|required|absent|missing|empty|case|insensitive|sensitive|upper\w*|lower\w*|whitespace|trim\w*|order|first|before|after|wins|precedence|never|always|not|no|none|ignored|reject\w*|logs?|logged)\b`)

// arithRe matches numbers and arithmetic, outside the citations.
var arithRe = regexp.MustCompile(`\d|[*/×÷→=<>]`)

// pageWeight puts the pages that state rules ahead of those that describe
// layout when the budget does not cover every claim.
var pageWeight = map[string]int{
	"docs/business-rules.md": 3,
	"docs/api.md":            2,
	"docs/errors.md":         2,
	"docs/configuration.md":  1,
	"docs/request-flow.md":   1,
}

// Rank orders claims for judging: most checklist risk first, then page
// order and line. It returns indices into claims.
func Rank(claims []verify.Claim) []int {
	score := make([]int, len(claims))
	for i, c := range claims {
		text := c.Text
		for _, ct := range c.Citations {
			text = strings.ReplaceAll(text, ct.Ref, " ")
		}
		seen := map[string]bool{}
		for _, w := range riskRe.FindAllString(text, -1) {
			seen[strings.ToLower(w)] = true
		}
		score[i] = len(seen) + pageWeight[c.Page]
		if arithRe.MatchString(text) {
			score[i] += 2
		}
	}
	order := make([]int, len(claims))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return score[b] - score[a] })
	return order
}
