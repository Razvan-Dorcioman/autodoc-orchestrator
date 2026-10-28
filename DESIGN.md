# DESIGN — autodoc-orchestrator

Orchestrator that points at any Go HTTP service (developed against `catalog-poc`) and produces (1) project documentation and (2) swaggo annotations + a generated OpenAPI spec, without changing service behaviour.

**Stack:** Go · Google ADK for Go (pinned) · one `llm.Provider`: a local open-weight model over an OpenAI-compatible API (default: Ollama + `qwen3:14b`, ≥16k context) or Gemini (`AUTODOC_PROVIDER=gemini`; Flash for agents, Pro for the judge) · own JSONL trace.

## Key decisions

- **Deterministic pipeline with LLM steps**, not one free-roaming agent. LLMs only write annotations, write docs and fact-check claims; fetching, parsing, verifying and committing are plain Go: cheaper, reproducible, testable.
- **One agent per deliverable, swagger first.** Annotations and docs need different context and checks; the docs agent then cites the *verified* spec instead of re-deriving it — one source of truth.
- **ADK inside each LLM step, not for the pipeline.** ADK runs the model ↔ tool loop over typed `functiontool`s, so a tool error becomes an in-loop correction; its callbacks feed the trace and enforce caps. Order, gates, retries and reset stay in plain Go.
- **Facts from type-checked code, nothing configured per target.** `index` extracts `facts.json` from stdlib `net/http` idioms (matched by type): routes, params and the defaults/enums/bounds handlers enforce, JSON bodies, status codes and body types, constants, env reads, Make targets. Gates check output against these facts.
- **Code can't change, by construction and after the fact.** The agent's only write tool, `patch_comment`, sets a function's `// @` lines (by file + func name, never line number); anything else is a tool error. Trade-off: no struct tags, so no per-field `example`/`required`.

## Flow

```
fetch ─► index ─► strip ─► SWAGGER AGENT ─► gate A ─► add route ─► reindex ─► DOCS AGENT ─► gate B ─► JUDGE ─► commit
 det      det      det      LLM              det        det          det       LLM            det     LLM (Pro)   det
                              ▲                │                                  ▲             │        │
                              └─ errors (≤3) ──┘                                  └─ ≤3 / 2 ────┴────────┘
```

`fetch` clones into `runs/<id>/work` (never touching the original) on branch `autodoc/<base-sha>`, requires a green baseline build and hashes every file. `index` also plans the extra route and rejects a repo with its own spec, so unsupported services fail before any LLM call. `strip` removes old annotations for idempotent re-runs; `reindex` refreshes line numbers after annotation. The docs agent writes eight `docs/` pages. `commit` re-checks staged paths and makes one commit, which is pushed back to the target as branch `autodoc/<base-sha>`: always into a local repo (its working tree untouched), to a URL only with `AUTODOC_GIT_TOKEN` (never in args or config).

**The extra route** is the only intended behaviour change: `internal/apispec/` `go:embed`s the generated spec, and the route-registering function gains one `mux.Handle("GET /openapi.json", apispec.Handler())` line plus the import. Template, not LLM; stdlib only.

## Context strategy

- **Swagger agent:** one turn per handler with only its facts, types, constants and source. Small context → fewer invented params.
- **Docs agent:** one session per page, so prompts fit a 16k local context and gate B's issues return to the session that wrote it. It gets the brief, the facts it must mention and a capped slice of `facts.json`; claims cite `file:symbol`.
- **Judge:** one claim + its cited snippet per call, fresh session. Claims are split deterministically by citation; `// @` lines are removed from snippets (LLM-written, not evidence).
- **Prompt caching:** each agent's system prompt and tools are constant and come first, so provider prefix caches (Gemini implicit caching, the local server's KV cache) reuse them and the growing session history; a test fails if anything per run gets in. Judge messages start with the cited code, and claims citing the same code are judged back to back (risk still picks which).

## Verification

Deterministic, all must pass:
- **Code unchanged:** every `.go` file's comment-free token hash equals the baseline, except `internal/apispec/` and the template edit; `go.mod` unchanged.
- **Build:** the orchestrator's own `go build ./...` and `go vet ./...`; green at baseline must stay green. Repo tests run only with `--run-tests` and never block (they may need DBs or secrets).
- **Spec:** pinned swag → valid Swagger 2.0, refs resolve, regeneration byte-identical; operations, params, bodies, status codes and fields match the facts — nothing invented, nothing missing. Param defaults, enums and bounds are checked where the code makes them certain; on the last attempt those alone are advisory.
- **Docs:** every citation resolves to real code, and each fact has a page that must name it.

Model-judged: the judge rates each claim *supported / unsupported / unverifiable* against its code, with a generic checklist of subtle errors (units, rounding, defaults, validation order); up to 60 claims per pass, riskiest first. Only *unsupported* goes back to the docs agent. The judge gets two passes; the second is **advisory** — it is a model and can be wrong, so remaining findings are committed for review and flagged in `judge.json`, `summary.json` and the commit message.

**On failure:** gate errors go back to the producing agent in the same session (≤3 gate A runs, ≤3 in a row for gate B). When attempts, the token budget (`AUTODOC_TOKEN_BUDGET`, default 1M; catalog-poc runs used ≤~270k) or the per-agent call cap (25) run out, the workdir is reset to baseline, nothing is committed, and the run exits non-zero with the gate report. Model errors: ≤3 retries with backoff on 429/5xx/timeouts, then the next model in the step's chain; auth and bad-request errors aren't retried.

## Re-runnability and trace

Everything generated is regenerated each run, so an unchanged service gives a near-empty diff, and a code change alters `facts.json`, failing the gates until annotations and docs catch up.

`trace.jsonl` logs every step, agent turn, tool call, model call (model used, tokens incl. prompt-cache hits, latency, estimated cost), retry, fallback and gate result; `summary.json` has the status, per-step and per-model totals (cost at `AUTODOC_PRICES`, plus at a reference paid price, since the defaults are free) and the result.

## Known limitations

- Quality is only as good as the judge; it can't judge readability or fitness for a new engineer.
- Only stdlib `net/http` routing (chi/gin/echo need extractors); non-constant patterns, statuses or param names are missed; form, multipart and header inputs aren't extracted.
- Swagger 2.0: no `oneOf` (`any` fields are free-form objects); no struct tags means no per-field examples or `required`; `ServeMux` 404/405 only in prose.
- Regenerate, not update: wording churns between runs. `swag` needs network on first run. Single repo, Go only, no incremental mode, no MR.

## With one more week

- Golden evaluation set: expected facts and questions answered from the docs alone, scored automatically — measures *good*, not just *valid*.
- MR mode: changed packages only, gate report and judge verdicts as an MR comment.
- Monorepo scale: per-package fan-out, docs per service + index.
- Contract tests: call the running service with spec-generated requests and compare responses.
- OTel/Langfuse exporter on top of the JSONL trace.

## How AI assistants were used

- **Tool:** Claude Code only (Opus 5.5; Sonnet 5.5 for one commit); late on, parallel sessions on one checkout, each owning separate files.
- **Split:** the design (pipeline vs. agents, gates, what counts as evidence) was worked out in conversation; the assistant wrote the code, tests and commit messages, one flow step per commit.
- **Verification:** every diff read before commit; `go test` plus end-to-end catalog-poc runs, checking the trace, gate reports and target diff.
- **Where it fell short:** it overfit to catalog-poc (target-specific shortcuts, undone in `415f504`); it made gates stricter than a free model could pass, so the judge's last pass became advisory; and it wrote at length, which put this file over its page limit.
