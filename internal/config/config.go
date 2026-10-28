// Package config reads the orchestrator's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"autodoc/internal/llm"
	"autodoc/internal/trace"
)

// Default model chains per provider, first choice first. Each step falls back
// along its chain when a model is overloaded, rate-limited, timing out or retired.
var (
	defaultGeminiAgentModels = []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.5-flash"}
	defaultGeminiJudgeModels = []string{"gemini-pro-latest", "gemini-3.1-pro-preview", "gemini-3.8-flash"}

	// One model fits a 16 GB GPU at a time, so the local judge is the agents' model.
	defaultLocalModels = []string{"qwen3:14b"}
)

const (
	defaultLocalURL = "http://localhost:11434/v1" // Ollama
	// Without a key, openai-go would send OPENAI_API_KEY from the environment to the local server.
	placeholderLocalKey = "local"
	// A local model may load from disk and then generate on a small GPU for minutes.
	defaultLocalTimeout = 10 * time.Minute
	// Runs on catalog-poc used up to about 270k tokens, so this leaves room
	// for retries and fallbacks but stops a run that loops.
	defaultTokenBudget = 1_000_000
)

// Config holds everything the orchestrator needs from the environment.
type Config struct {
	Provider    llm.Provider
	AgentModels []string // AUTODOC_AGENT_MODEL: swagger and docs agents, comma-separated fallback chain
	JudgeModels []string // AUTODOC_JUDGE_MODEL: grounding judge, comma-separated fallback chain
	MaxCalls    int      // model calls allowed per agent before it is stopped
	TokenBudget int      // AUTODOC_TOKEN_BUDGET: tokens the whole run may use, every agent together; 0 means no limit
	GitToken    string   // AUTODOC_GIT_TOKEN: when set, the run branch is pushed to an https --repo with it (a local --repo needs none)
	// AUTODOC_PRICES and AUTODOC_REFERENCE_PRICE: the price table for the
	// trace's estimated cost. Models not in it count as free.
	Pricing trace.Pricing
}

// Load reads .env in the working directory, if present, then the environment.
// AUTODOC_PROVIDER picks the model backend: "local" (default) or "gemini".
// It fails only when the chosen provider is misconfigured.
func Load() (Config, error) {
	if err := LoadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	cfg := Config{MaxCalls: 25, GitToken: strings.TrimSpace(os.Getenv("AUTODOC_GIT_TOKEN"))}

	kind := strings.ToLower(strings.TrimSpace(os.Getenv("AUTODOC_PROVIDER")))
	if kind == "" {
		kind = llm.ProviderLocal
	}
	agentDefault, judgeDefault := defaultLocalModels, defaultLocalModels
	switch kind {
	case llm.ProviderGemini:
		cfg.Provider = llm.Provider{Kind: kind, APIKey: os.Getenv("GOOGLE_API_KEY")}
		agentDefault, judgeDefault = defaultGeminiAgentModels, defaultGeminiJudgeModels
		if cfg.Provider.APIKey == "" {
			return Config{}, errors.New("GOOGLE_API_KEY is not set (see .env.example)")
		}
	case llm.ProviderLocal:
		cfg.Provider = llm.Provider{
			Kind:    kind,
			APIKey:  strOr("AUTODOC_LOCAL_API_KEY", placeholderLocalKey),
			BaseURL: strOr("AUTODOC_LOCAL_URL", defaultLocalURL),
			Timeout: defaultLocalTimeout,
		}
	default:
		return Config{}, fmt.Errorf("AUTODOC_PROVIDER=%q is not supported (use %q or %q)", kind, llm.ProviderLocal, llm.ProviderGemini)
	}

	if s := strings.TrimSpace(os.Getenv("AUTODOC_HTTP_TIMEOUT")); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("AUTODOC_HTTP_TIMEOUT=%q is not a positive duration such as 90s or 15m", s)
		}
		cfg.Provider.Timeout = d
	}

	cfg.TokenBudget = defaultTokenBudget
	if s := strings.TrimSpace(os.Getenv("AUTODOC_TOKEN_BUDGET")); s != "" {
		n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("AUTODOC_TOKEN_BUDGET=%q is not a token count such as 500000 (0 for no limit)", s)
		}
		cfg.TokenBudget = n
	}

	pricing, err := loadPricing()
	if err != nil {
		return Config{}, err
	}
	cfg.Pricing = pricing

	cfg.AgentModels = listOr("AUTODOC_AGENT_MODEL", agentDefault)
	cfg.JudgeModels = listOr("AUTODOC_JUDGE_MODEL", judgeDefault)
	return cfg, nil
}

// loadPricing reads AUTODOC_PRICES, comma-separated model=in/out entries in
// USD per 1M tokens (e.g. "gemini-2.5-flash=0.30/2.50"), and
// AUTODOC_REFERENCE_PRICE, one name=in/out entry that replaces the default
// reference.
func loadPricing() (trace.Pricing, error) {
	p := trace.DefaultPricing()
	parse := func(key, entry string) (string, trace.Price, error) {
		name, price, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return "", trace.Price{}, fmt.Errorf("%s: %q is not model=in/out, such as gemini-2.5-flash=0.30/2.50", key, entry)
		}
		pr, err := trace.ParsePrice(price)
		if err != nil {
			return "", trace.Price{}, fmt.Errorf("%s: %s: %w", key, name, err)
		}
		return name, pr, nil
	}
	for _, entry := range listOr("AUTODOC_PRICES", nil) {
		name, pr, err := parse("AUTODOC_PRICES", entry)
		if err != nil {
			return p, err
		}
		if p.Models == nil {
			p.Models = map[string]trace.Price{}
		}
		p.Models[name] = pr
	}
	if s := strings.TrimSpace(os.Getenv("AUTODOC_REFERENCE_PRICE")); s != "" {
		name, pr, err := parse("AUTODOC_REFERENCE_PRICE", s)
		if err != nil {
			return p, err
		}
		p.ReferenceName, p.Reference = name, pr
	}
	return p, nil
}

func strOr(key, fallback string) string {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return s
	}
	return fallback
}

// listOr splits a comma-separated variable, dropping empty entries.
func listOr(key string, fallback []string) []string {
	var out []string
	for _, s := range strings.Split(os.Getenv(key), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
