package config

import (
	"slices"
	"testing"
	"time"

	"autodoc/internal/llm"
)

// clearEnv blanks every variable Load reads, so a developer's shell cannot leak in.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AUTODOC_PROVIDER", "AUTODOC_LOCAL_URL", "AUTODOC_LOCAL_API_KEY", "AUTODOC_HTTP_TIMEOUT",
		"AUTODOC_AGENT_MODEL", "AUTODOC_JUDGE_MODEL", "GOOGLE_API_KEY", "AUTODOC_TOKEN_BUDGET", "AUTODOC_GIT_TOKEN",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaultsToLocal(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Provider
	if p.Kind != llm.ProviderLocal || p.BaseURL != "http://localhost:11434/v1" || p.APIKey == "" || p.Timeout != 10*time.Minute {
		t.Errorf("provider = %+v", p)
	}
	if !slices.Equal(cfg.AgentModels, defaultLocalModels) || !slices.Equal(cfg.JudgeModels, defaultLocalModels) {
		t.Errorf("models = %v / %v, want the local default", cfg.AgentModels, cfg.JudgeModels)
	}
	if cfg.TokenBudget != defaultTokenBudget || cfg.GitToken != "" {
		t.Errorf("token budget %d, git token %q", cfg.TokenBudget, cfg.GitToken)
	}
}

func TestLoadLocalOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTODOC_LOCAL_URL", "http://gpu-box:8080/v1")
	t.Setenv("AUTODOC_HTTP_TIMEOUT", "30m")
	t.Setenv("AUTODOC_AGENT_MODEL", "gpt-oss:20b, qwen3:14b")
	t.Setenv("AUTODOC_TOKEN_BUDGET", "500_000")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.BaseURL != "http://gpu-box:8080/v1" || cfg.Provider.Timeout != 30*time.Minute {
		t.Errorf("provider = %+v", cfg.Provider)
	}
	if want := []string{"gpt-oss:20b", "qwen3:14b"}; !slices.Equal(cfg.AgentModels, want) {
		t.Errorf("agent models = %v, want %v", cfg.AgentModels, want)
	}
	if cfg.TokenBudget != 500000 {
		t.Errorf("token budget = %d", cfg.TokenBudget)
	}
}

func TestLoadGeminiNeedsKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("AUTODOC_PROVIDER", "gemini")
	if _, err := Load(); err == nil {
		t.Fatal("want an error without GOOGLE_API_KEY")
	}

	t.Setenv("GOOGLE_API_KEY", "k")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Kind != llm.ProviderGemini || cfg.Provider.APIKey != "k" || cfg.Provider.Timeout != 0 {
		t.Errorf("provider = %+v", cfg.Provider)
	}
	if !slices.Equal(cfg.AgentModels, defaultGeminiAgentModels) || !slices.Equal(cfg.JudgeModels, defaultGeminiJudgeModels) {
		t.Errorf("models = %v / %v, want the gemini defaults", cfg.AgentModels, cfg.JudgeModels)
	}
}

func TestLoadRejectsBadSettings(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown provider": {"AUTODOC_PROVIDER": "openai"},
		"bad timeout":      {"AUTODOC_HTTP_TIMEOUT": "soon"},
		"zero timeout":     {"AUTODOC_HTTP_TIMEOUT": "0s"},
		"bad budget":       {"AUTODOC_TOKEN_BUDGET": "1M"},
		"negative budget":  {"AUTODOC_TOKEN_BUDGET": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Error("want an error")
			}
		})
	}
}
