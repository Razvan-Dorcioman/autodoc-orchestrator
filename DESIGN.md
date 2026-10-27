# DESIGN — autodoc-orchestrator

Orchestrator that points at a Go service (`catalog-poc`) and produces (1) project documentation and (2) swaggo annotations + a generated OpenAPI spec, without changing service behaviour.

**Stack:** Go · Google ADK for Go (`google.golang.org/adk/v2`, exact version pinned) · Gemini via AI Studio (`GOOGLE_API_KEY`) · Flash for agents, Pro for the judge · own JSONL trace.

**Why ADK, and where it stops.** ADK is used *inside* each LLM step, not for the pipeline. Each agent is an `llmagent` whose tools are typed `functiontool`s (schemas inferred from Go structs), and ADK runs the model ↔ tool loop. A tool error goes back to the model as the function response, so `patch_comment` rejections become in-loop corrections for free. `Before/AfterModelCallback` and `Before/AfterToolCallback` feed the trace: per-call `UsageMetadata` (tokens), tool arguments, results and errors. The pipeline — step order, gates, outer retries with gate feedback, reset — is plain Go, because it must be deterministic and testable. Stop caps (max model calls per agent, token budget) are a `BeforeModelCallback` that counts and returns an error. The plain `genai` SDK would mean hand-writing the function-calling loop, the tool schemas and the hooks; that's about the same amount of code ADK already gives us, minus the tested edge cases.

**Model availability.** Each LLM step has a model chain (agents: `gemini-3.8-flash → 3.7-flash → 3.5-flash`; judge: `gemini-pro-latest → 3.1-pro-preview → 3.8-flash`; overridable via env).
- **HTTP level:** each request gets a 90 s deadline and up to 3 attempts with exponential backoff on 429, 500, 502, 503, 504 and timeouts, honouring `Retry-After`. Each retry is a `retry` event in the trace.
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
| fetch | det | Clone the URL or local path (a local path is cloned too, so the original is never touched) into `runs/<id>/work`, branch `autodoc/<base-sha>`, baseline `go build/vet/test` (must be green, or the run stops), snapshot every file: SHA-256 + for `.go` files a code hash of the token stream without comments → `baseline.json` | git, go |
| index | det | `go/ast` extraction → `facts.json` (see below) | — |
| strip | det | Remove every `// @…` swag line from `.go` files and the generated outputs → idempotent re-runs | — |
| Swagger agent | LLM | swaggo `// @` annotations on the 3 handlers + general API info on `main` | `read_file`, `list_symbols`, `get_facts`, `patch_comment` |
| Gate A | det | See *Verification*; structured errors fed back to the agent | go, swag |
| add route | det | Template-insert `GET /openapi.json` (see below) + a test for it | — |
| Docs agent | LLM | `docs/*.md`: overview, structure, request flow, business rules, error model, configuration, test & ship | `read_file`, `get_facts`, `get_spec`, `write_doc` (scoped to `docs/`) |
| Gate B | det | See *Verification* | — |
| Judge | LLM | Split docs into claims; verify each against its cited code; failed claims go back to the docs agent | `read_file` |
| commit | det | One separate commit on the branch; push / open MR only if a token is set; write run summary | git |

CLI: `autodoc run --repo <url|path>` → working copy in a temp dir, output in `runs/<run-id>/`.

**`facts.json` — what `index` extracts** (tuned to the stdlib idioms catalog-poc uses):
- Routes from `mux.HandleFunc("<METHOD> <pattern>", h.fn)`, Go 1.22 patterns incl. `{$}` (exact `/`) and `{id}`.
- Inputs per handler: `r.PathValue("…")`, `query.Get("…")`; related constants (`defaultQty = 1`, `maxQty = 10`) and enums (`Currency` consts `RON|EUR|HUF`).
- Outputs per handler: status + error code from every `writeError(w, http.StatusX, "code", …)`, response type from every `writeJSON` call, JSON fields of those types (incl. `omitempty`).
- Implicit responses: unmatched path → `404` and wrong method → `405` from `ServeMux` as `text/plain`, not the JSON error envelope; panics → `500 internal` from `recoverPanics`.
- Config: `envOr("PORT", "8080")`, server timeouts, Make targets, Dockerfile stages.

**The extra route.** `GET /openapi.json` lets clients and tooling fetch the spec from the running service. Swag writes the spec into `internal/apispec/` next to a tiny `apispec.go` that `go:embed`s it — inside `internal/` because the Dockerfile copies only `go.mod`, `cmd/` and `internal/`, so a root-level `docs/` would not reach the image. `router.go` gains exactly one `mux.Handle("GET /openapi.json", apispec.Handler())` line + import. Template-inserted, not LLM-written; stdlib only (no swaggo runtime / http-swagger). This is the only intended behaviour change.

## Context strategy

- **Swagger agent:** one handler at a time + its request/response types + its `facts.json` entries + a short swaggo cheat sheet. Never sees docs. Small context → fewer hallucinated params.
- **Docs agent:** `facts.json` + the verified spec as primary sources; `read_file` for details (`pricing.go`, `product.go`, `middleware.go`, Makefile, Dockerfile). It is told to cite `file:symbol` for every non-trivial claim.
- **Judge:** one claim + its cited snippet per call. Cheap, focused, and the verdict is attributable to a specific line of code.
- Deterministic steps pass artifacts (`facts.json`, spec, gate reports) — agents never get raw logs of earlier steps.

## Verification

Deterministic (all must pass):
- **Code unchanged — by construction:** `patch_comment` accepts only *inserting* lines that start with `// @` directly above a func declaration; anything else is a tool error the agent sees and corrects (visible in the trace). Consequence: no struct tags, so no `example:` / `enums:` / `required` on response fields — parameter constraints go in `@Param` attributes instead (`Enums(RON,EUR,HUF) default(RON)`, `minimum(1) maximum(10) default(1)`).
- **Code unchanged — after the fact:** the code hash of every `.go` file equals the baseline. It hashes the Go token stream with comments and layout dropped, so comments and formatting don't count but any change to code, identifiers, literals or struct tags does; the only allowed exceptions are the new `internal/apispec/` package and the exact expected one-line diff in `router.go`. `go.mod` unchanged (no `go.sum` exists and none may appear).
- **Build:** `go build ./...`, `go vet ./...`, `go test ./...` pass (`make check` covers only vet + test). `docker build` too, when Docker is available.
- **Spec generation:** pinned swag v1 → Swagger 2.0: `swag init -g cmd/server/main.go --parseInternal --outputTypes json,yaml -o internal/apispec` (no `docs.go`, so no new dependency). Output must be valid; regenerating is byte-identical.
- **Spec ↔ code grounding:** paths, methods, path/query params, defaults, enums, min/max, status codes + error codes and response field names in the spec match `facts.json` exactly — nothing invented, nothing missing.
- **Docs ↔ code grounding:** every referenced file, symbol, route, env var and Make target exists; every route, error code, query param, response field and business-rule constant in `facts.json` is mentioned.
- **Scope:** only `// @` lines in `.go` files, `docs/`, `internal/apispec/` and the `router.go` line changed.

Model-judged:
- **Claim fact-check (Pro):** each docs claim judged *supported / unsupported / unverifiable* against its cited code. Unsupported claims are sent back with the judge's reason. The judge gets a checklist of rules that are easy to get subtly wrong:
  - money is integer RON bani; the discount rounds **down** (integer division); conversion happens only when the response is built;
  - fixed indicative FX rates (1 RON = 0.2012 EUR = 79.45 HUF), rounded half away from zero to 2 / 2 / 0 decimals;
  - `total` = discounted unit price × qty in bani, converted once (not converted unit price × qty);
  - `GET /` always prices in RON; `list_price` / `discount_percent` appear only for discounted products;
  - `in_stock` = stock > 0; `available` = stock ≥ qty; out-of-stock products still return 200; `qty` is capped at 10 regardless of stock;
  - `currency` is case-insensitive and trimmed; validation order is id → currency → qty → lookup (an invalid qty on a missing id gives 400, not 404);
  - access logs deliberately omit query strings and client IPs (GDPR) — a "safe to change" constraint.

**On failure:** errors are fed back to the producing agent (≤3 attempts for swagger, ≤2 for docs). If attempts run out, the workdir is reset to the baseline, nothing is committed, and the run exits non-zero with the gate report.

**Stop conditions:** all gates green; retry limit reached; token/cost budget exceeded; per-agent tool-call cap reached.

## Re-runnability

- Idempotent regeneration: all `// @` lines are stripped and rewritten each run; `docs/` and `internal/apispec/` are regenerated in full; the route template is a no-op if the line is already present.
- On an unchanged service, a second run should produce a near-empty diff. After a code change (e.g. a new `query.Get("…")` or a new response field), `facts.json` changes, so gate A fails until the annotation exists and gate B fails until the docs mention it.

## Trace

- `runs/<run-id>/trace.jsonl`: one event per step start/end, tool call (args, result size, error), model call (model, tokens in/out, latency, estimated cost), retry, gate result.
- `runs/<run-id>/summary.json`: totals per step and per model, retries, final status, approximate cost (price table in code).

## Known limitations

- The judge is a model: "good" is only as good as its fact-check; it can't judge readability or whether docs suit a new engineer.
- `facts.json` extraction targets the stdlib idioms of this repo (`ServeMux` patterns, `query.Get`, `writeError`/`writeJSON`); other routers or helpers need new extractors.
- Swagger 2.0 has no `oneOf`: `specs` (`any`, one of `LaptopSpecs` / `BookSpecs` / `ApparelSpecs` by category) is described as a free-form object plus a description. Swag v2 / OpenAPI 3.1 would fix this but is not stable.
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
- Push target for the deliverable run (fork vs local branch only).
- `go.mod` says `go 1.22` while the Dockerfile builds with `golang:1.26`; the docs should state 1.22 as the minimum (needed for method/wildcard `ServeMux` patterns).
- How AI coding assistants were used to build the orchestrator (to fill in at the end).
