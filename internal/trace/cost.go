package trace

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Price is what a model costs, in USD per million tokens. CachedInputPerM is
// the rate for input tokens the provider served from its prompt cache.
type Price struct {
	InputPerM       float64 `json:"input_per_m"`
	CachedInputPerM float64 `json:"cached_input_per_m"`
	OutputPerM      float64 `json:"output_per_m"`
}

func (p Price) String() string {
	if p.CachedInputPerM == p.InputPerM {
		return fmt.Sprintf("$%g/$%g per 1M in/out", p.InputPerM, p.OutputPerM)
	}
	return fmt.Sprintf("$%g/$%g/$%g per 1M in/cached/out", p.InputPerM, p.CachedInputPerM, p.OutputPerM)
}

// ParsePrice reads "in/out" or "in/cached/out", USD per million input, cached
// input and output tokens, such as "1.25/10" or "1.25/0.125/10". Without a
// cached rate, cached input costs as much as any other input.
func ParsePrice(s string) (Price, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 2 && len(parts) != 3 {
		return Price{}, fmt.Errorf("price %q is not in/out or in/cached/out USD per 1M tokens, such as 1.25/10 or 1.25/0.125/10", s)
	}
	v := make([]float64, len(parts))
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || f < 0 {
			return Price{}, fmt.Errorf("price %q is not in/out or in/cached/out USD per 1M tokens, such as 1.25/10 or 1.25/0.125/10", s)
		}
		v[i] = f
	}
	if len(v) == 2 {
		return Price{InputPerM: v[0], CachedInputPerM: v[0], OutputPerM: v[1]}, nil
	}
	return Price{InputPerM: v[0], CachedInputPerM: v[1], OutputPerM: v[2]}, nil
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
// uses Gemini 2.5 Pro's paid list price (prompts up to 200k tokens; cache hits
// at a 90% discount) as the reference.
func DefaultPricing() Pricing {
	return Pricing{ReferenceName: "gemini-2.5-pro", Reference: Price{InputPerM: 1.25, CachedInputPerM: 0.125, OutputPerM: 10}}
}

// costNote goes into summary.json and run_end so nobody reads the figure as a bill.
const costNote = "estimate: recorded token counts times the configured price table (AUTODOC_PRICES); " +
	"models not in it are counted as free; cached input is priced at the cached rate; thought tokens are priced as output; not a bill"

// cost returns the estimated and reference cost of one call. cached is the
// part of in that came from the provider's prompt cache. Thought tokens are
// billed as output by the APIs that report them.
func (p Pricing) cost(model string, in, cached, out int) (est, ref float64) {
	cached = min(max(cached, 0), in)
	price := func(pr Price) float64 {
		return (float64(in-cached)*pr.InputPerM + float64(cached)*pr.CachedInputPerM + float64(out)*pr.OutputPerM) / 1e6
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
