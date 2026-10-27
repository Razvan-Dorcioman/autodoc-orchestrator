// Package config reads the orchestrator's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Default model chains, first choice first. Each step falls back along its
// chain when a model is overloaded, rate-limited, timing out or retired.
var (
	defaultAgentModels = []string{"gemini-3.8-flash", "gemini-3.7-flash", "gemini-3.5-flash"}
	defaultJudgeModels = []string{"gemini-pro-latest", "gemini-3.1-pro-preview", "gemini-3.8-flash"}
)

// Config holds everything the orchestrator needs from the environment.
type Config struct {
	APIKey      string   // GOOGLE_API_KEY (AI Studio)
	AgentModels []string // AUTODOC_AGENT_MODEL: swagger and docs agents, comma-separated fallback chain
	JudgeModels []string // AUTODOC_JUDGE_MODEL: grounding judge, comma-separated fallback chain
	MaxCalls    int      // model calls allowed per agent before it is stopped
}

// Load reads .env in the working directory, if present, then the environment.
// It fails only when the API key is missing.
func Load() (Config, error) {
	if err := LoadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	cfg := Config{
		APIKey:      os.Getenv("GOOGLE_API_KEY"),
		AgentModels: listOr("AUTODOC_AGENT_MODEL", defaultAgentModels),
		JudgeModels: listOr("AUTODOC_JUDGE_MODEL", defaultJudgeModels),
		MaxCalls:    25,
	}
	if cfg.APIKey == "" {
		return Config{}, errors.New("GOOGLE_API_KEY is not set (see .env.example)")
	}
	return cfg, nil
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
