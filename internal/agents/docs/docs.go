// Package docs is the LLM agent that writes the project documentation under docs/.
//
// Each page has its own session, so a prompt holds only that page's facts
// and the spec, which keeps it within a local model's context. The pipeline
// writes the pages in order, then sends each page's failed gate B issues
// back to the session that wrote it.
package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
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
	"autodoc/internal/verify"
)

// Step is the trace step name of the agent's calls.
const Step = "docs"

const instruction = `You write the documentation of a Go HTTP service, for an engineer who is new to the team.

Each turn asks for one Markdown page under docs/. Save it with write_doc: the path and the complete page.
Your reply text is discarded: a page exists only once write_doc has been called with it, so never print the
page as text. Calling write_doc again replaces the page, so always pass all of it.

Ground every statement in the code. The primary sources are the facts in the message, extracted from the
type-checked source, and, for endpoints, parameters, status codes and error codes, the verified OpenAPI spec
(in the message or from get_spec). Use read_file for what the message lacks: function bodies, middleware,
README.md, the Makefile, the Dockerfile. Never invent endpoints, parameters, fields, error codes, environment
variables, Make targets, files or behaviour; if the code does not show something, leave it out. Model calls
are slow, so read only what the page needs.

Citations: back every non-trivial claim with a citation in backticks: the file's path from the repository
root, a colon and a symbol declared in that file, for example ` + "`internal/shop/orders.go:(*Store).Get`" + `,
` + "`internal/shop/orders.go:maxItems`" + ` or ` + "`Makefile:test`" + `. A deterministic gate checks every citation, every
file path, every ` + "`METHOD /path`" + `, every ` + "`make <target>`" + ` and every environment variable you write in
backticks or code blocks, so use only ones that exist.

The message lists facts the page must mention: name each with the exact spelling given, in backticks, and
explain it.

Cite code in backticks, never as Markdown links: a link from docs/ to "internal/x.go:12" does not resolve.
Use links only for the other pages, as [API](api.md), instead of repeating them.

Style: start with "# <Title>"; short sections; tables for parameters, fields, errors and settings. Plain and
precise.

After writing, reply with one short line saying what the page covers.`

// page is what the agent is told about one page.
type page struct {
	title string
	about string
	facts func(a *Agent) []section // the facts the message carries
}

type section struct {
	title string
	value any
}

var pages = map[string]page{
	"overview.md": {
		title: "Overview",
		about: "What the service does and for whom, the endpoints at a glance (one line each, linking to api.md), " +
			"how to run it locally, and a list of the other pages with one line each. Read README.md if the repository has one.",
		facts: func(a *Agent) []section {
			return []section{
				{"Module", map[string]string{"module": a.facts.Module, "go_version": a.facts.GoVersion}},
				{"Commands (main packages)", a.facts.Mains},
				{"Packages", packageBriefs(a.facts, false)},
				{"Routes", routeBriefs(a.facts)},
				{"Environment variables", a.facts.Env},
				{"Repository files", a.files},
			}
		},
	},
	"structure.md": {
		title: "Structure",
		about: "The repository layout: each package's directory, its responsibility, its key files and symbols, and how " +
			"the packages depend on each other; the commands; where a new engineer should start reading.",
		facts: func(a *Agent) []section {
			return []section{
				{"Packages", packageBriefs(a.facts, true)},
				{"Commands (main packages)", a.facts.Mains},
				{"Functions", functionBriefs(a.facts)},
				{"Types", typeBriefs(a.facts)},
				{"Repository files", a.files},
			}
		},
	},
	"request-flow.md": {
		title: "Request flow",
		about: "What happens to a request from the listener to the response: the http.Server settings, where the routes " +
			"are registered, each middleware in the order a request passes it and what it does, the handler, and the " +
			"helpers that write JSON and errors. Read the route-registering function and the middleware.",
		facts: func(a *Agent) []section {
			var routes []map[string]any
			for _, r := range a.facts.Routes {
				routes = append(routes, map[string]any{"pattern": r.Pattern, "handler": r.Handler, "handler_pos": r.HandlerPos, "registered": r.Registered, "calls": r.Calls})
			}
			return []section{
				{"Routes", routes},
				{"Server settings", a.facts.ServerSettings},
				{"Responses the mux writes itself", a.facts.ImplicitResponses},
				{"Functions", functionBriefs(a.facts)},
			}
		},
	},
	"api.md": {
		title: "API",
		about: "Every endpoint: method and path, what it does, its path and query parameters with type, default, allowed " +
			"values and bounds, its status codes with their error codes, and its response fields with type and meaning " +
			"(say when a field is omitted). Take parameters and statuses from the spec. Add an example request per " +
			"endpoint. Also document GET /openapi.json, which serves this spec as JSON.",
		facts: func(a *Agent) []section {
			var bodies []string
			for _, r := range a.facts.Routes {
				for _, s := range r.Responses {
					if s.Status < 400 {
						bodies = append(bodies, s.Body)
					}
				}
			}
			out := []section{{"Routes", routeBriefs(a.facts)}}
			if spec, err := tools.GetSpec(a.spec, tools.GetSpecArgs{}); err == nil {
				out = append(out, section{"The verified spec (operations and the definitions they use)", spec})
			}
			return append(out, section{"Response types (from the Go source; omitempty fields are left out when empty)", a.facts.Reachable(bodies...)})
		},
	},
	"business-rules.md": {
		title: "Business rules",
		about: "The domain logic behind the endpoints, precisely: validation rules and the order they are checked in, " +
			"defaults and caps, every value of each enum, and each calculation step by step with its units, integer " +
			"or decimal arithmetic, rounding direction and precision, and the order of operations; fields that appear " +
			"only under a condition; case and whitespace handling. Read the functions that implement them.",
		facts: func(a *Agent) []section {
			var routes []map[string]any
			for _, r := range a.facts.Routes {
				routes = append(routes, map[string]any{"pattern": r.Pattern, "handler_pos": r.HandlerPos, "params": r.Params, "consts_used": r.ConstsUsed, "calls": r.Calls})
			}
			return []section{
				{"Routes", routes},
				{"Constants", a.facts.Constants},
				{"Package-level variables", a.facts.Vars},
				{"Functions", functionBriefs(a.facts)},
				{"Tests (their names often state a rule)", a.facts.Tests},
			}
		},
	},
	"errors.md": {
		title: "Errors",
		about: "The error model: the error body and its fields, every error code with its status, when it happens and its " +
			"message; which error wins when several apply; what a panic or an unexpected error returns; and the " +
			"plain-text responses the mux writes itself (unknown path, wrong method).",
		facts: func(a *Agent) []section {
			var bodies []string
			for _, s := range a.facts.Responses {
				if s.Status >= 400 {
					bodies = append(bodies, s.Body)
				}
			}
			return []section{
				{"Every place a status is written (handlers and middleware)", a.facts.Responses},
				{"Responses the mux writes itself", a.facts.ImplicitResponses},
				{"Error body types", a.facts.Reachable(bodies...)},
			}
		},
	},
	"configuration.md": {
		title: "Configuration",
		about: "Everything that configures the running service: each environment variable with its default and effect, " +
			"the http.Server settings and timeouts, the listen address, and the container settings from the " +
			"Dockerfile. Only what the code and files show.",
		facts: func(a *Agent) []section {
			return []section{
				{"Environment variables", a.facts.Env},
				{"Server settings", a.facts.ServerSettings},
				{"Dockerfile", a.facts.Dockerfile},
				{"Make targets", a.facts.MakeTargets},
			}
		},
	},
	"testing.md": {
		title: "Test and ship",
		about: "How to build, vet, test and run the service locally; what the tests cover, by name; every Make target and " +
			"what it runs; building and running the container image; the Go version.",
		facts: func(a *Agent) []section {
			return []section{
				{"Module", map[string]string{"module": a.facts.Module, "go_version": a.facts.GoVersion}},
				{"Make targets", a.facts.MakeTargets},
				{"Tests", a.facts.Tests},
				{"Dockerfile", a.facts.Dockerfile},
			}
		},
	},
}

// Agent writes the docs pages of one working copy, one session per page.
type Agent struct {
	cfg      config.Config
	rec      *trace.Recorder
	dir      string
	facts    *gofacts.Facts
	spec     map[string]any
	files    []string // the repository's files, for the layout pages
	model    *llm.Fallback
	tools    []tool.Tool
	scope    *tools.DocScope
	mentions map[string][]verify.Mention
	sessions map[string]*pageSession
}

type pageSession struct {
	r  *runner.Runner
	id string
}

// New builds the agent with read_file, get_facts, get_spec and write_doc,
// all scoped to dir. The spec must have passed gate A.
func New(ctx context.Context, cfg config.Config, rec *trace.Recorder, dir string, facts *gofacts.Facts) (*Agent, error) {
	m, err := llm.NewModelChain(ctx, cfg.AgentModels, cfg.Provider, rec)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	spec, err := tools.LoadSpec(abs)
	if err != nil {
		return nil, err
	}
	readFile, err := tools.NewReadFile(abs)
	if err != nil {
		return nil, err
	}
	getFacts, err := tools.NewGetFacts(facts)
	if err != nil {
		return nil, err
	}
	getSpec, err := tools.NewGetSpec(abs)
	if err != nil {
		return nil, err
	}
	scope := &tools.DocScope{}
	writeDoc, err := tools.NewWriteDoc(abs, scope)
	if err != nil {
		return nil, err
	}
	files, err := repoFiles(abs)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg: cfg, rec: rec, dir: abs, facts: facts, spec: spec, files: files, model: m,
		tools: []tool.Tool{readFile, getFacts, getSpec, writeDoc}, scope: scope,
		mentions: verify.DocMentions(facts), sessions: map[string]*pageSession{},
	}, nil
}

// maxNudges bounds how often a page turn is repeated after the agent
// answered in text without calling write_doc.
const maxNudges = 2

// Write runs one turn per page, each in the page's own session.
func (a *Agent) Write(ctx context.Context) error {
	for _, p := range verify.DocPages {
		msg, err := a.pageMessage(p)
		if err != nil {
			return err
		}
		if err := a.writeTurn(ctx, p, msg); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	return nil
}

// Fix sends the gate B issues of one page back to the session that wrote it.
func (a *Agent) Fix(ctx context.Context, page string, issues []verify.Issue) error {
	rep := verify.Report{Gate: "B", Issues: issues}
	msg := fmt.Sprintf("Gate B checked %s and failed. Fix every issue and call write_doc again with path %q and the "+
		"complete corrected page. Keep what was right.\n\n%s", page, page, rep)
	return a.writeTurn(ctx, page, msg)
}

// writeTurn runs a turn that must end with write_doc having been called,
// asking again if the model answered in text instead.
func (a *Agent) writeTurn(ctx context.Context, p, msg string) error {
	s, err := a.session(p)
	if err != nil {
		return err
	}
	a.scope.Set(p)
	defer a.scope.Clear()
	for nudge := 0; ; nudge++ {
		written, err := a.turn(ctx, s, msg)
		if err != nil || written > 0 {
			return err
		}
		if nudge == maxNudges {
			// Leave it to gate B, which reports the missing page.
			return nil
		}
		a.rec.Emit(trace.Event{Type: trace.Retry, Step: Step, Agent: "docs_agent",
			Detail: fmt.Sprintf("answered without calling write_doc for %s; asking again (%d of %d)", p, nudge+1, maxNudges)})
		msg = fmt.Sprintf("You answered in text and did not call write_doc, so nothing was saved. Call the write_doc "+
			"tool now with path %q and the complete page in content. Do not reply with the page as text.", p)
	}
}

// session returns the page's session, building its agent on first use. Each
// page's agent has its own call cap; the model chain is shared, so a
// fallback holds for the rest of the run.
func (a *Agent) session(p string) (*pageSession, error) {
	if s, ok := a.sessions[p]; ok {
		return s, nil
	}
	agentCfg := llmagent.Config{
		Name:        "docs_agent",
		Description: "Writes one page of the project documentation.",
		Model:       a.model,
		// A provider, not Instruction: ADK would read "{id}"-style text as a session-state placeholder.
		InstructionProvider: func(agent.ReadonlyContext) (string, error) { return instruction, nil },
		Tools:               a.tools,
	}
	hooks := &llm.Hooks{Rec: a.rec, Step: Step, Model: a.model, MaxCalls: a.cfg.MaxCalls}
	hooks.Apply(&agentCfg)
	ag, err := llmagent.New(agentCfg)
	if err != nil {
		return nil, fmt.Errorf("build agent: %w", err)
	}
	r, err := runner.New(runner.Config{
		AppName:           "autodoc",
		Agent:             ag,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return nil, fmt.Errorf("build runner: %w", err)
	}
	s := &pageSession{r: r, id: a.rec.RunID() + "-" + Step + "-" + strings.TrimSuffix(path.Base(p), ".md")}
	a.sessions[p] = s
	return s, nil
}

// turn sends one user message and returns how many write_doc calls the
// agent made (rejected ones included).
func (a *Agent) turn(ctx context.Context, s *pageSession, msg string) (written int, err error) {
	content := genai.NewContentFromText(msg, genai.RoleUser)
	var answer string
	for ev, err := range s.r.Run(ctx, "autodoc", s.id, content, agent.RunConfig{StreamingMode: agent.StreamingModeNone}) {
		if err != nil {
			return 0, fmt.Errorf("agent run: %w", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.Name == "write_doc" {
				written++
			}
			if ev.IsFinalResponse() {
				answer += p.Text
			}
		}
	}
	a.rec.Emit(trace.Event{Type: trace.Turn, Step: Step, Agent: "docs_agent",
		Args: map[string]any{"session": s.id, "message_bytes": len(msg), "write_calls": written}, Detail: strings.TrimSpace(answer)})
	return written, nil
}

// pageMessage is the request for one page: what it covers, the facts it
// must mention, the other pages, and the facts it is written from.
func (a *Agent) pageMessage(p string) (string, error) {
	pg, ok := pages[path.Base(p)]
	if !ok {
		return "", fmt.Errorf("no brief for page %s", p)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Write %s, titled %q. Call write_doc with path %q.\n\n", p, pg.title, p)
	fmt.Fprintf(&b, "It covers: %s\n\n", pg.about)
	if ms := a.mentions[p]; len(ms) > 0 {
		b.WriteString("It must mention (exact spelling, in backticks):\n")
		for _, m := range ms {
			alt := ""
			if len(m.Alts) > 0 {
				alt = " or `" + strings.Join(m.Alts, "`, `") + "`"
			}
			fmt.Fprintf(&b, "- %s `%s`%s (%s)\n", m.Kind, m.Text, alt, m.Source)
		}
		b.WriteString("\n")
	}
	b.WriteString("The other pages, to link to instead of repeating them:\n")
	for _, other := range verify.DocPages {
		if other != p {
			o := pages[path.Base(other)]
			fmt.Fprintf(&b, "- [%s](%s): %s\n", o.title, path.Base(other), firstSentence(o.about))
		}
	}
	b.WriteString("\n")
	for _, s := range pg.facts(a) {
		data, err := json.MarshalIndent(s.value, "", " ")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s:\n```json\n%s\n```\n\n", s.title, data)
	}
	return b.String(), nil
}

func firstSentence(s string) string {
	if i := strings.Index(s, ": "); i > 0 && i < 60 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}

func routeBriefs(f *gofacts.Facts) []map[string]any {
	var out []map[string]any
	for _, r := range f.Routes {
		out = append(out, map[string]any{"pattern": r.Pattern, "handler": r.Handler, "handler_pos": r.HandlerPos, "params": r.Params})
	}
	return out
}

func packageBriefs(f *gofacts.Facts, withFiles bool) []map[string]any {
	var out []map[string]any
	for _, p := range f.Packages {
		m := map[string]any{"path": p.Path, "dir": p.Dir}
		if p.Doc != "" {
			m["doc"] = p.Doc
		}
		if withFiles {
			m["files"] = p.Files
		}
		out = append(out, m)
	}
	return out
}

func functionBriefs(f *gofacts.Facts) []map[string]any {
	var out []map[string]any
	for _, fn := range f.Functions {
		m := map[string]any{"name": fn.Package + "." + fn.Name, "signature": fn.Signature, "pos": fn.Pos}
		if fn.Doc != "" {
			m["doc"] = fn.Doc
		}
		out = append(out, m)
	}
	return out
}

func typeBriefs(f *gofacts.Facts) []map[string]any {
	var out []map[string]any
	for _, t := range f.Types {
		m := map[string]any{"name": t.Package + "." + t.Name, "underlying": t.Underlying, "pos": t.Pos}
		if t.Doc != "" {
			m["doc"] = t.Doc
		}
		out = append(out, m)
	}
	return out
}

// maxFiles bounds the file list in a message; a larger repository is
// described by its packages.
const maxFiles = 300

// repoFiles lists the files of the working copy, without .git and the docs
// pages being written.
func repoFiles(dir string) ([]string, error) {
	pages := map[string]bool{}
	for _, p := range verify.DocPages {
		pages[p] = true
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !pages[rel] && len(out) < maxFiles {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}
