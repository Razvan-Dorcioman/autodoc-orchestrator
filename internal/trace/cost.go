package trace

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Price is what a model costs, in USD per million tokens.
type Price struct {
	InputPerM  float64 `json:"input_per_m"`
	OutputPerM float64 `json:"output_per_m"`
}

func (p Price) String() string {
	return fmt.Sprintf("$%g/$%g per 1M in/out", p.InputPerM, p.OutputPerM)
}

// ParsePrice reads "in/out", USD per million input and output tokens, such as "1.25/10".
func ParsePrice(s string) (Price, error) {
	in, out, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return Price{}, fmt.Errorf("price %q is not in/out USD per 1M tokens, such as 1.25/10", s)
	}
	var p Price
	var err1, err2 error
	p.InputPerM, err1 = strconv.ParseFloat(strings.TrimSpace(in), 64)
	p.OutputPerM, err2 = strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err1 != nil || err2 != nil || p.InputPerM < 0 || p.OutputPerM < 0 {
		return Price{}, fmt.Errorf("price %q is not in/out USD per 1M tokens, such as 1.25/10", s)
	}
	return p, nil
}

// Pricing estimates what model calls cost. A model missing from Models is
// taken to be free (a local model, or a free API tier) and costs $0. Every
// call is also priced at Reference, so a run on free models still says what
// it would cost on a paid one.
type Pricing struct {
	Models        map[string]Price
	ReferenceName string
	Reference     Price
}

// DefaultPricing has no paid models, since the default chains are free, and
// uses Gemini 2.5 Pro's paid list price (prompts up to 200k tokens) as the
// reference.
func DefaultPricing() Pricing {
	return Pricing{ReferenceName: "gemini-2.5-pro", Reference: Price{InputPerM: 1.25, OutputPerM: 10}}
}

// costNote goes into summary.json and run_end so nobody reads the figure as a bill.
const costNote = "estimate: recorded token counts times the configured price table (AUTODOC_PRICES); " +
	"models not in it are counted as free; thought tokens are priced as output; not a bill"

// cost returns the estimated and reference cost of one call. Thought tokens
// are billed as output by the APIs that report them.
func (p Pricing) cost(model string, in, out int) (est, ref float64) {
	price := func(pr Price) float64 {
		return (float64(in)*pr.InputPerM + float64(out)*pr.OutputPerM) / 1e6
	}
	if pr, ok := p.Models[model]; ok {
		est = price(pr)
	}
	return est, price(p.Reference)
}

// usd rounds to a millionth of a dollar, enough for a single small call.
func usd(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// CostSummary is the run's estimated cost in summary.json.
type CostSummary struct {
	Note           string   `json:"note"`
	USD            float64  `json:"usd"`
	ReferenceModel string   `json:"reference_model"`
	ReferencePrice Price    `json:"reference_price"`
	ReferenceUSD   float64  `json:"reference_usd"`           // what the same tokens would cost at the reference price
	FreeModels     []string `json:"free_models,omitempty"`   // models used that are not in the price table
	PricedModels   []string `json:"priced_models,omitempty"` // models used that are
}

func (r *Recorder) costSummary() CostSummary {
	c := CostSummary{Note: costNote, USD: usd(r.estUSD), ReferenceModel: r.pricing.ReferenceName,
		ReferencePrice: r.pricing.Reference, ReferenceUSD: usd(r.refUSD)}
	for m := range r.models {
		if m == "" {
			continue // a call that failed before a model was named
		}
		if _, ok := r.pricing.Models[m]; ok {
			c.PricedModels = append(c.PricedModels, m)
		} else {
			c.FreeModels = append(c.FreeModels, m)
		}
	}
	slices.Sort(c.FreeModels)
	slices.Sort(c.PricedModels)
	return c
}
