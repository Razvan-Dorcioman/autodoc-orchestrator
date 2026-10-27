// Package smoke runs one tiny agent end to end: a Gemini model, the read_file
// tool and the trace hooks. It proves the wiring before the real agents exist.
package smoke

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"autodoc/internal/config"
	"autodoc/internal/llm"
	"autodoc/internal/tools"
	"autodoc/internal/trace"
)

const instruction = `You answer questions about a Go repository.
Use the read_file tool to read files; never guess file contents.
Answer in one line.`

const question = "Read go.mod and reply with the module path and the go directive version, as: <module> <version>"

// Run asks the agent to read go.mod in dir and returns its final answer.
func Run(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir string) (string, error) {
	const step = "smoke"
	rec.Emit(trace.Event{Type: trace.StepStart, Step: step, Detail: dir})
	answer, err := run(ctx, cfg, rec, dir, step)
	end := trace.Event{Type: trace.StepEnd, Step: step}
	if err != nil {
		end.Error = err.Error()
	}
	rec.Emit(end)
	return answer, err
}

func run(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir, step string) (string, error) {
	m, err := llm.NewModelChain(ctx, cfg.AgentModels, cfg.APIKey, rec)
	if err != nil {
		return "", err
	}
	readFile, err := tools.NewReadFile(dir)
	if err != nil {
		return "", err
	}

	agentCfg := llmagent.Config{
		Name:        "smoke_agent",
		Description: "Reads go.mod and reports the module path.",
		Model:       m,
		Instruction: instruction,
		Tools:       []tool.Tool{readFile},
	}
	hooks := &llm.Hooks{Rec: rec, Step: step, Model: m, MaxCalls: 5}
	hooks.Apply(&agentCfg)
	a, err := llmagent.New(agentCfg)
	if err != nil {
		return "", fmt.Errorf("build agent: %w", err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "autodoc",
		Agent:             a,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return "", fmt.Errorf("build runner: %w", err)
	}

	msg := genai.NewContentFromText(question, genai.RoleUser)
	var answer string
	for ev, err := range r.Run(ctx, "autodoc", rec.RunID(), msg, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			return "", fmt.Errorf("agent run: %w", err)
		}
		if ev == nil || !ev.IsFinalResponse() || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			answer += p.Text
		}
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", errors.New("agent returned no answer")
	}
	return answer, nil
}
