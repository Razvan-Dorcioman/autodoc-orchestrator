# DESIGN — autodoc-orchestrator

Orchestrator that points at any Go HTTP service (developed against `catalog-poc`) and produces (1) project documentation and (2) swaggo annotations + a generated OpenAPI spec, without changing service behaviour.

**Stack:** Go · Google ADK for Go (`google.golang.org/adk/v2`, exact version pinned) · models behind one `llm.Provider`: a local open-weight model over an OpenAI-compatible API (default, Ollama + `qwen3:14b`) or Gemini via AI Studio (`AUTODOC_PROVIDER=gemini`; Flash for agents, Pro for the judge) · own JSONL trace.

**Why ADK, and where it stops.** ADK is used *inside* each LLM step, not for the pipeline. Each agent is an `llmagent` whose tools are typed `functiontool`s (schemas inferred from Go structs), and ADK runs the model ↔ tool loop. A tool error goes back to the model as the function response, so `patch_comment` rejections become in-loop corrections for free. `Before/AfterModelCallback` and `Before/AfterToolCallback` feed the trace: per-call `UsageMetadata` (tokens), tool arguments, results and errors. The pipeline — step order, gates, outer retries with gate feedback, reset — is plain Go, because it must be deterministic and testable. Stop caps (max model calls per agent, token budget) are a `BeforeModelCallback` that counts and returns an error. The plain `genai` SDK would mean hand-writing the function-calling loop, the tool schemas and the hooks; that's about the same amount of code ADK already gives us, minus the tested edge cases.

**Model availability.** Each LLM step has a model chain, overridable via env (local: `qwen3:14b` for both steps; Gemini agents: `gemini-3.8-flash → 3.7-flash → 3.5-flash`, judge: `gemini-pro-latest → 3.1-pro-preview → 3.8-flash`). Local models run through ADK's `openaimodel` (Chat Completions), so the agents, tools, hooks and fallback are provider-agnostic; the server needs a ≥16k context (Ollama's default truncates prompts silently).
- **HTTP level:** each request gets a deadline (90 s for Gemini, 10 min locally, `AUTODOC_HTTP_TIMEOUT`) and up to 3 attempts with exponential backoff on 429, 500, 502, 503, 504 and timeouts, honouring `Retry-After` or Gemini's `RetryInfo.retryDelay` up to 60 s; a longer delay (a daily quota) skips straight to the next model. Each retry is a `retry` event in the trace.
- **Model level:** if a model still fails with overload, rate limit, timeout or 404 (retired), the step switches to the next model in its chain and stays on it for the rest of the run, recording a `fallback` event. Bad requests and auth errors are not retried, since another model would fail the same way.
- `model_call` events log the model actually used, so the trace shows which outputs came from a fallback model.

## Why this split

- **Deterministic pipeline with LLM steps**, not one free-roaming agent. LLMs are used only where the work is prose or judgement: writing annotations, writing docs, fact-checking claims. Fetching, parsing, verifying and committing are plain Go: cheaper, reproducible, testable.
- **One agent per deliverable.** Annotations and docs need different context and different checks; separating them keeps prompts small and failures attributable.
- **Sequential: swagger first, docs second.** The docs agent cites the *verified* spec for parameters and errors instead of re-deriving them, so there is one source of truth and fewer contradictions.

## Flow

```
fetch ─► index ─► strip ─► SWAGGER AGENT ─► gate A ─► add route ─► DOCS AGENT ─► gate B ─► JUDGE ─► commit
 det      det      det      LLM (Flash)      det        det         LLM (Flash)    det     LLM (Pro)   det
                              ▲                │                      ▲             │        │
                              └─ errors (≤3) ──┘                      └─ errors (≤2)┴────────┘
```

| Step | Kind | Responsibility | Tools |
|---|---|---|---|
| fetch | det | Clone the URL or local path (a local path is cloned too, so the original is never touched) into `runs/<id>/work`, branch `autodoc/<base-sha>`, baseline `go build` (must be green, or the run stops) and `go vet`, snapshot every file: SHA-256 + for `.go` files a code hash of the token stream without comments → `baseline.json` | git, go |
| index | det | `go/ast` extraction → `facts.json` (see below); picks the command for the general API info: the one `main` package that registers a route or imports a package that does (`--main` overrides); plans the extra route, so a service the template can't handle fails before any LLM call | — |
| strip | det | Remove every `// @…` / `//@…` line comment on its own line (tokenized, so strings are safe) from `.go` files, plus `swagger.json`/`.yaml` and a swag-generated `docs.go` in `internal/apispec/` → idempotent re-runs | — |
| Swagger agent | LLM | swaggo `// @` annotations on every route handler + general API info on that command's `main` | `read_file`, `get_facts`, `patch_comment` |
| Gate A | det | See *Verification*; structured errors fed back to the agent | go, swag |
| add route | det | Template-insert `GET /openapi.json` (see below); checked by the orchestrator (`route.json`), no test added to the repo | — |
| Docs agent | LLM | Eight `docs/` pages: overview, structure, request flow, API, business rules, errors, configuration, test & ship | `read_file`, `get_facts`, `get_spec`, `write_doc` (one page per turn) |
| Gate B | det | See *Verification* | — |
| Judge | LLM | Split docs into claims; verify each against its cited code; failed claims go back to the docs agent | — (the cited code is in the message) |
| commit | det | One commit by `autodoc` on `autodoc/<base-sha>`, after re-checking that every staged path is a non-test `.go` file, `internal/apispec/` or a docs page; nothing changed → no commit, status `unchanged`. With `AUTODOC_GIT_TOKEN`, push the branch to the https origin (token in an env-scoped `http.extraHeader`, never in args or `.git/config`; `--force-with-lease`, so a re-run replaces its own branch); a failed push keeps the local commit and fails the run. No MR is opened | git |

CLI: `autodoc run --repo <url|path> [--main <file>] [--run-tests]` → working copy in a temp dir, output in `runs/<run-id>/`. Nothing about the target is configured: everything comes from its type-checked code.

**`facts.json` — what `index` extracts** (stdlib `net/http` idioms, matched by type, not by name):
- Routes from `HandleFunc` / `Handle` with a constant Go 1.22 pattern, incl. `{$}` and `{id}`; the `main` packages and which of them serve the routes. A pattern without a method: a handler that is a `switch r.Method` with one module function per case becomes one route per method on that function; otherwise the route keeps the methods its handler compares `r.Method` with (none: it answers every method).
- Inputs per handler: `r.PathValue("…")`, `url.Values.Get/Has("…")`, a JSON request body (`json.Decoder.Decode` or `json.Unmarshal` where `r.Body` is read, also one level into a helper such as `decode(r, &in)`); the constants it uses and their enum siblings.
- Reach: each function and constant gets its call depth from the nearest handler (a constant also via its named type), so lists too long for a prompt keep what the API uses.
- Outputs: every call passing an `http.Status*` constant (handlers and middleware), with the string codes after it and the body type, followed one level into helpers (e.g. a `writeError`); JSON fields of those types (incl. `omitempty`).
- Implicit responses: unmatched path → `404` and wrong method → `405` from `ServeMux` as `text/plain`.
- Config: env reads (direct or via a helper like `envOr`), `http.Server` fields, Make targets, Dockerfile.

**The extra route.** `GET /openapi.json` lets clients and tooling fetch the spec from the running service. Swag writes the spec into `internal/apispec/` next to a tiny `apispec.go` that `go:embed`s it — a package of the module, so it reaches any build or image that compiles the service. The function that registers the routes gains exactly one `mux.Handle("GET /openapi.json", apispec.Handler())` line after its last registration, plus the import; nothing else in the file moves. So the routes must be registered in one function on one mux, and none may already answer `GET /openapi.json`. Template-inserted, not LLM-written; stdlib only (no swaggo runtime / http-swagger). This is the only intended behaviour change. Checked by: a temporary self-test in `internal/apispec/` (`go test`, then deleted) that the handler serves `swagger.json` as JSON; re-extracted facts showing the route registered once, on a `net/http` mux, in the planned file; the code guard against the original with the template applied; and build/vet. On a re-run the route is already there (the insert is a no-op) and is left out of `facts.json`.

## Context strategy

- **Swagger agent:** one ADK session, one turn per handler: the message carries its route facts, the types it writes (transitively), the constants it uses with their enum siblings and the handler source; the instruction holds the rules and a short swaggo cheat sheet. Then one turn for the general info on `main`; failed gate reports come back as further turns. Never sees docs. Small context → fewer hallucinated params.
- **Docs agent:** one session per page, so a prompt fits a 16k local context and gate B's issues for a page go back to the session that wrote it. The message carries the page's brief, the facts gate B requires it to mention, and the slice of `facts.json` it is written from (the verified spec for the API page; function, type and variable lists capped at 80/60/40, nearest to the handlers first, with a count of the rest for `get_facts`); `read_file` for details (the functions handlers call, middleware, Makefile, Dockerfile). It is told to cite `file:symbol` for every non-trivial claim.
- **Judge:** one claim + its cited snippet per call, in a fresh session. Cheap, focused, and the verdict is attributable to a specific line of code. The split is deterministic: a claim is a line (sentence, bullet or table row) citing (in backticks or `[…]`) `file:symbol`, `file:line`, `pkg.Symbol`, `make x` or `METHOD /path` (its handler); a line without a citation inherits its parent list item's, else its section heading's. The snippet is the cited declaration plus the package's consts, vars and types it uses, with `// @` lines removed (LLM-written, so not evidence). Uncited prose lines are not judged, only listed in `judge.json`.
- Deterministic steps pass artifacts (`facts.json`, spec, gate reports) — agents never get raw logs of earlier steps.

## Verification

Deterministic (all must pass):
- **Code unchanged — by construction:** `patch_comment` (file + func name, never a line number) only sets a function's `// @Name…` lines directly above its func line, after any doc comment; its earlier `// @` lines are replaced, so a retry can correct them. Anything else (other lines, test files, a code-hash change) is a tool error the agent sees and corrects (visible in the trace). Consequence: no struct tags, so no `example:` / `enums:` / `required` on response fields — parameter constraints go in `@Param` attributes instead (`Enums(a,b) default(a)`, `minimum(1) maximum(10)`).
- **Code unchanged — after the fact:** the code hash of every `.go` file equals the baseline. It hashes the Go token stream with comments and layout dropped, so comments and formatting don't count but any change to code, identifiers, literals or struct tags does; the only allowed exceptions are the new `internal/apispec/` package and the template's exact edit of the route-registering file (its original with the route line and import inserted). A changed `.go` file must also equal its base-commit version once `// @` lines are stripped from both, so existing comments and layout stay. `go.mod` unchanged (no `go.sum` exists and none may appear).
- **Build:** the orchestrator's own checks, never the repo's scripts: `go build ./...` and `go vet ./...` (which compiles the tests without running them). A check green at the baseline must stay green; one already red (say, an existing vet finding) doesn't block. The repo's own tests run only with `--run-tests` and are reported, never blocking: they may need a database, network or secrets, and the code guard already proves no code changed.
- **Spec generation:** swag v1.16.6 via `go run` → Swagger 2.0: `swag init -g <main file> --parseInternal --outputTypes json,yaml -o internal/apispec` (no `docs.go`, so no new dependency). Valid = Swagger 2.0 with title/version, every `$ref` resolves, YAML says the same as JSON; regenerating into a temp dir is byte-identical.
- **Spec ↔ code grounding:** the same operations (`basePath` + path, one per method; a route that ignores the method needs at least one, each checked) and path/query params (path ones required) as `facts.json`, a required body param with the decoded type exactly when the handler decodes one, exactly the status codes each handler writes with the facts' body type (`{array}` for a slice), every error code named in its status's description, and definition properties = the types' JSON names — nothing invented, nothing missing. Defaults, enums and min/max are required by the instruction but not checked here: facts don't tie a param to its constants.
- **Docs ↔ code grounding:** every reference resolves: `file:symbol` (or `file:line`) to a file and a declaration in it, `pkg.Symbol` to a module package, `METHOD /path` to a route, `make x` and `Makefile:x` to a target, an env var in code to one the code reads or the Makefile/Dockerfile names, a relative link to a file. Each fact has one page that must name it: routes, params, request- and success-body fields on the API page; error codes, error-body fields and the mux's 4xx on errors; constants the handlers reach on business rules (at most 40, nearest first); env vars and server settings on configuration; Make targets on test & ship; package dirs on structure.
- **Scope:** only `// @` lines in `.go` files, `docs/`, `internal/apispec/` and the one route line changed; the docs step changes only its pages.

Model-judged:
- **Claim fact-check (Pro):** each docs claim judged *supported / unsupported / unverifiable* against its cited code. Unsupported claims are sent back with the judge's reason. The judge gets a generic checklist of things that are easy to get subtly wrong, applied to whatever the cited code does: units and integer vs. decimal arithmetic, rounding direction, order of operations (convert then multiply, or the reverse), defaults and caps, fields that appear only under a condition (`omitempty`), validation order (which error wins when several apply), case and whitespace handling, and deliberate omissions such as data not logged. Only *unsupported* fails; *unverifiable* (the cited code doesn't decide it) is reported. Budget: 60 claims per pass, riskiest first (checklist words, numbers, rule pages); the rest are reported as skipped. Verdicts are cached by line + snippet, so a re-check after a fix judges only what changed. Gate B and the judge share the ≤2 docs attempts; after a fix gate B must pass again before the judge runs.

**On failure:** errors are fed back to the producing agent in the same session (≤3 gate runs for swagger, ≤2 for docs). If attempts run out, the workdir is reset to the baseline (`git reset --hard` + `git clean -fd`), nothing is committed, and the run exits non-zero with the gate report (also in `gate_a.json`).

**Stop conditions:** all gates green; retry limit reached; token/cost budget exceeded; per-agent tool-call cap reached; judge claim budget reached (remaining claims skipped).

## Re-runnability

- Idempotent regeneration: all `// @` lines are stripped and rewritten each run; the docs pages are deleted and rewritten and `internal/apispec/` is regenerated in full; the route template is a no-op if the line is already present.
- On an unchanged service, a second run should produce a near-empty diff. After a code change (e.g. a new `query.Get("…")` or a new response field), `facts.json` changes, so gate A fails until the annotation exists and gate B fails until the docs mention it.

## Trace

- `runs/<run-id>/trace.jsonl`: one event per step start/end, agent turn (final reply), tool call (args, result size, error), model call (model, tokens in/out, latency, estimated cost), retry, gate result.
- `runs/<run-id>/summary.json`: status (`committed`, `unchanged`, `failed` with the failed step and error, `incomplete` if interrupted), totals, each step's duration, error and model calls/tokens, calls and tokens per model, and the result (source, base SHA, branch, commit, files, pushed-to URL). No cost estimate: the default models are free.

## Known limitations

- The judge is a model: "good" is only as good as its fact-check; it can't judge readability or whether docs suit a new engineer.
- `facts.json` extraction covers stdlib `net/http` (`ServeMux` patterns, `PathValue`, `url.Values`, `http.Status*` constants); routers like chi, gin or echo need their own extractors. Non-constant patterns, statuses or param names are missed. A method-less handler that handles several methods inline gets one annotation block, so its methods share params, body and responses. Form, multipart and header inputs are not extracted.
- Swagger 2.0 has no `oneOf`: a field typed `any` or an interface is described as a free-form object plus a description. Swag v2 / OpenAPI 3.1 would fix this but is not stable.
- Comment-only edits mean no struct tags → no per-field examples or `required` markers on response schemas.
- The `404`/`405` plain-text responses from `ServeMux` are documented in prose, not in the spec.
- Business-rule coverage is checked against constants and a checklist, not semantics.
- Re-runs regenerate rather than update; docs wording may churn between runs even when the code didn't change.
- `swag` is fetched with `go run` at a pinned version → needs network access on the first run.
- Single repo, single language (Go), no incremental mode.

## With one more week

- Golden evaluation set: hand-written expected facts and doc questions ("what happens when `qty` exceeds stock?") answered from docs only, scored automatically — measures *good*, not just *valid*.
- MR mode: run on changed packages only, post the gate report and judge verdicts as an MR comment, cache `facts.json` per package.
- Monorepo scale: per-package fan-out with a bounded worker pool, docs per service + an index, context from a symbol graph instead of file reads.
- Contract tests: hit the running service with requests generated from the spec and compare responses with the documented shapes and errors.
- Langfuse/OTel exporter on top of the JSONL trace.

## Open items

- Hard per-run token/cost budget value (TBD).
- Opening an MR/PR after the push.
- How AI coding assistants were used to build the orchestrator (to fill in at the end).
