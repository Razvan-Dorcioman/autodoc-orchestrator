// Package swagger is the LLM agent that writes swaggo annotations.
//
// The pipeline drives it turn by turn in one session: one turn per route
// handler, with only that handler's facts, types, constants and source in
// the message, then one turn for the general API info on main, then one
// turn per failed gate A run with the gate report.
package swagger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"autodoc/internal/config"
	"autodoc/internal/gofacts"
	"autodoc/internal/llm"
	"autodoc/internal/tools"
	"autodoc/internal/trace"
)

// Step is the trace step name of the agent's calls.
const Step = "swagger"

const instruction = `You write swaggo (swag v1) annotations for a Go HTTP service built on net/http.

You cannot change code. Your only edit is patch_comment, which sets the "// @" annotation lines above one
top-level function; calling it again for the same function replaces that function's annotations.
Your reply text is discarded: annotations you only write in a reply are lost. An annotation exists once
patch_comment has been called with it, so never print annotations as text.

Ground every annotation in what the code does. Never invent parameters, status codes, headers, fields or
examples. Each handler message already holds the route facts, the types it writes, the constants it uses and
the handler source: do not read them again. Call read_file or get_facts only for something the message lacks,
such as a called function whose code decides a parameter's rules. Model calls are rate-limited, so go
straight to patch_comment when you can.

Rules for a handler:
- One @Param per path and query parameter listed in the route facts, and no others. Path params are
  required (true). Query params are optional (false) when the code falls back to a default.
- Put the constraints the code enforces in @Param attributes: Enums(A,B,C) for a fixed set of accepted
  values, default(x) for the value used when the parameter is absent, minimum(n) / maximum(n) for numeric
  bounds the code checks. The route facts list the ones they could read per param (default, minimum,
  maximum, enum, each with its position); the gate checks those. Use type int for values parsed with
  strconv.Atoi.
- When the route facts have a body, the handler decodes the request body as JSON: add one
  @Param body body <body.type> true "<what it carries>" and @Accept json. Without a body, neither.
- One @Success or @Failure line per status code in the route's responses, and no other codes. Use
  {object} and the package-qualified body type from the facts, e.g. {object} httpapi.ErrorResponse, or
  {array} and the element type for a slice body: []shop.Order is {array} shop.Order.
  When one status has several error codes, write one line and name every error code in its description.
- @Router lines: the facts' path, one line per method, lowercase. The method is the route's method. When
  the route has none, ServeMux sends every method to the handler: write one line per entry in methods
  (the methods the handler checks r.Method for); if methods is empty too, the handler ignores the method,
  so write one line for each method a client is meant to use, judging by what the handler does.
- Field details come from the types' JSON tags; you cannot add struct tags.
- Use @Produce json. Do not add @Security, @Header, @host or @schemes.

Syntax (an unrelated example):
  // @Summary      Get an order
  // @Description  Returns one order. The status filter is case-insensitive.
  // @Tags         orders
  // @Produce      json
  // @Param        id      path   int     true   "Order ID"       minimum(1)
  // @Param        status  query  string  false  "Order status"   Enums(open,closed) default(open)
  // @Param        limit   query  int     false  "Maximum items"  minimum(1) maximum(50) default(20)
  // @Success      200  {object}  shop.OrderResponse
  // @Failure      400  {object}  shop.ErrorResponse  "bad_id or bad_status: the id or status is invalid"
  // @Router       /orders/{id} [get]
A route with a request body and two methods (only the lines that differ):
  // @Accept       json
  // @Param        body  body  shop.OrderUpdate  true  "The fields to change"
  // @Router       /orders/{id} [put]
  // @Router       /orders/{id} [patch]

General API info goes on func main only:
  // @title        <service name>
  // @version      <version>
  // @description  <what the service does>
  // @BasePath     /

After patching, reply with one short line saying what you annotated.`

// Agent is one swagger agent session over a working copy.
type Agent struct {
	r       *runner.Runner
	rec     *trace.Recorder
	dir     string
	main    string // file with func main, for the general API info
	facts   *gofacts.Facts
	scope   *tools.PatchScope // limits patch_comment to the function of an annotate turn
	session string
}

// New builds the agent with the read_file, get_facts and patch_comment tools,
// all scoped to dir. mainFile is the file with the func main that gets the
// general API info.
func New(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir, mainFile string, facts *gofacts.Facts) (*Agent, error) {
	m, err := llm.NewModelChain(ctx, cfg.AgentModels, cfg.Provider, rec)
	if err != nil {
		return nil, err
	}
	readFile, err := tools.NewReadFile(dir)
	if err != nil {
		return nil, err
	}
	getFacts, err := tools.NewGetFacts(facts)
	if err != nil {
		return nil, err
	}
	scope := &tools.PatchScope{}
	patch, err := tools.NewPatchComment(dir, scope)
	if err != nil {
		return nil, err
	}
	agentCfg := llmagent.Config{
		Name:        "swagger_agent",
		Description: "Writes swaggo annotations for the service's HTTP handlers.",
		Model:       m,
		// A provider, not Instruction: ADK would read "{object}" as a session-state placeholder.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) { return instruction, nil },
		Tools:               []tool.Tool{readFile, getFacts, patch},
	}
	hooks := &llm.Hooks{Rec: rec, Step: Step, Model: m, MaxCalls: cfg.MaxCalls, TokenBudget: cfg.TokenBudget}
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
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Agent{r: r, rec: rec, dir: abs, main: mainFile, facts: facts, scope: scope, session: rec.RunID() + "-" + Step}, nil
}

// maxNudges bounds how often one annotate turn is repeated after the agent
// answered in text without calling patch_comment.
const maxNudges = 2

// Annotate runs one turn per route handler, then one for the general API info.
func (a *Agent) Annotate(ctx context.Context) error {
	if len(a.facts.Routes) == 0 {
		return errors.New("facts.json lists no routes to annotate")
	}
	for _, r := range a.facts.Routes {
		msg, err := a.routeMessage(r)
		if err != nil {
			return err
		}
		file, _, _ := strings.Cut(r.HandlerPos, ":")
		if err := a.patchTurn(ctx, msg, file, shortName(r.Handler)); err != nil {
			return fmt.Errorf("annotate %s: %w", r.Pattern, err)
		}
	}
	if err := a.patchTurn(ctx, a.mainMessage(), a.main, "main"); err != nil {
		return fmt.Errorf("annotate main: %w", err)
	}
	return nil
}

// patchTurn runs a turn that must end with patch_comment having been called.
// Smaller models sometimes answer with the annotations as text, which saves
// nothing and, left in the session, teaches them to do it again; ask again
// right away rather than leaving it to a failed gate run. patch_comment only
// accepts fn in file during the turn, so copying the func of an earlier
// call is rejected in-loop and the model can correct it.
func (a *Agent) patchTurn(ctx context.Context, msg, file, fn string) error {
	a.scope.Set(file, fn)
	defer a.scope.Clear()
	for nudge := 0; ; nudge++ {
		_, patched, err := a.turn(ctx, msg)
		if err != nil || patched > 0 {
			return err
		}
		if nudge == maxNudges {
			// Leave it to gate A, which reports the missing annotations.
			return nil
		}
		a.rec.Emit(trace.Event{Type: trace.Retry, Step: Step, Agent: "swagger_agent",
			Detail: fmt.Sprintf("answered without calling patch_comment for %s; asking again (%d of %d)", fn, nudge+1, maxNudges)})
		msg = fmt.Sprintf("You answered in text and did not call patch_comment, so nothing was saved and the code has no annotations for this function. "+
			"Call the patch_comment tool now with file %q, func %q and the annotation lines in lines. Do not reply with the annotations as text.", file, fn)
	}
}

// Fix sends a failed gate report back in the same session.
func (a *Agent) Fix(ctx context.Context, report string) error {
	msg := "Gate A checked the annotations and the generated spec and failed. Fix every issue with patch_comment " +
		"(it replaces a function's annotation lines; pass the complete corrected set). Issues about code, scope or go " +
		"commands that annotations cannot fix: leave them and say so.\n\n" + report
	_, _, err := a.turn(ctx, msg)
	return err
}

// turn sends one user message and returns the agent's final text and how
// many patch_comment calls it made (rejected ones included).
func (a *Agent) turn(ctx context.Context, msg string) (answer string, patched int, err error) {
	content := genai.NewContentFromText(msg, genai.RoleUser)
	for ev, err := range a.r.Run(ctx, "autodoc", a.session, content, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			return "", 0, fmt.Errorf("agent run: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == "patch_comment" {
				patched++
			}
			if ev.IsFinalResponse() {
				answer += p.Text
			}
		}
	}
	answer = strings.TrimSpace(answer)
	a.rec.Emit(trace.Event{Type: trace.Turn, Step: Step, Agent: "swagger_agent", Args: map[string]any{"message_bytes": len(msg), "patch_calls": patched}, Detail: answer})
	return answer, patched, nil
}

// routeMessage is the context for one handler: its route facts, the types it
// writes (with the types they contain), the constants it uses (with the
// other values of their enum type) and its source.
func (a *Agent) routeMessage(r gofacts.Route) (string, error) {
	file, _, _ := strings.Cut(r.HandlerPos, ":")
	if file == "" {
		return "", fmt.Errorf("route %s: handler %s has no position in facts.json", r.Pattern, r.Handler)
	}
	fn := shortName(r.Handler)
	src, err := tools.FuncSource(a.dir, file, fn)
	if err != nil {
		return "", fmt.Errorf("route %s: %w", r.Pattern, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Annotate the handler of %s.\n", r.Pattern)
	fmt.Fprintf(&b, "Call patch_comment with file %q and func %q.\n\n", file, fn)
	writeJSON(&b, "Route facts (from the type-checked source; responses lists every status the handler writes)", r)
	writeJSON(&b, "Types it reads and writes", relatedTypes(a.facts, r))
	if cs := relatedConstants(a.facts, r); len(cs) > 0 {
		writeJSON(&b, "Constants it uses", cs)
	}
	fmt.Fprintf(&b, "Handler source (%s):\n```go\n%s\n```\n", r.HandlerPos, src)
	b.WriteString("\nThe functions it calls are listed in calls; read them with read_file or get_facts if a parameter's rules depend on them.\n")
	return b.String(), nil
}

func (a *Agent) mainMessage() string {
	return fmt.Sprintf(`Now add the general API info on func main in %s (patch_comment file %q, func "main"):
@title, @version, @description and @BasePath. Read README.md and the package docs for the service name and
what it does; keep the description to one or two sentences. For @version use the version README.md or the
code declares; if there is none, use 1.0. @BasePath is / because the routes are registered at the root.`,
		a.main, a.main)
}

// shortName drops the package prefix facts.json puts on handler names:
// "httpapi.(*handlers).product" becomes "(*handlers).product".
func shortName(handler string) string {
	if i := strings.Index(handler, ".("); i >= 0 {
		return handler[i+1:]
	}
	if i := strings.LastIndex(handler, "."); i >= 0 {
		return handler[i+1:]
	}
	return handler
}

// relatedTypes returns the request and response body types of r and,
// transitively, the module types their fields use.
func relatedTypes(f *gofacts.Facts, r gofacts.Route) []gofacts.Type {
	var bodies []string
	if r.Body != nil {
		bodies = append(bodies, r.Body.Type)
	}
	for _, s := range r.Responses {
		bodies = append(bodies, s.Body)
	}
	return f.Reachable(bodies...)
}

// relatedConstants returns the constants r uses and every constant sharing
// a named type with one of them, so an enum comes with all its values.
func relatedConstants(f *gofacts.Facts, r gofacts.Route) []gofacts.Constant {
	used := map[string]bool{}
	enumTypes := map[string]bool{}
	for _, name := range r.ConstsUsed {
		used[name] = true
		for _, c := range f.Constants {
			if c.Package+"."+c.Name == name && !strings.HasPrefix(c.Type, "untyped ") {
				enumTypes[c.Type] = true
			}
		}
	}
	var out []gofacts.Constant
	for _, c := range f.Constants {
		if used[c.Package+"."+c.Name] || enumTypes[c.Type] {
			out = append(out, c)
		}
	}
	return out
}

func writeJSON(b *strings.Builder, title string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		data = []byte(err.Error())
	}
	fmt.Fprintf(b, "%s:\n```json\n%s\n```\n\n", title, data)
}
