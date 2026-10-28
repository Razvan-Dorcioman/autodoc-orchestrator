# autodoc

Points at a Go HTTP service (developed against `catalog-poc`) and produces swaggo
annotations, a generated OpenAPI spec served at `GET /openapi.json`, and project
docs under `docs/`, without changing what the service does. LLM agents write;
deterministic gates (build/vet, swag, a code guard, docs checks) and an LLM judge
check their work. Every step is traced. How it works and why: [DESIGN.md](DESIGN.md).

## Prerequisites

- Go 1.27+ and `git` on `PATH`
- Network access for the first run: swag runs as `go run github.com/swaggo/swag/cmd/swag@<pinned version>`
- A model backend, either:
  - **local** (default): an OpenAI-compatible server, such as Ollama with `qwen3:14b` pulled and
    `OLLAMA_CONTEXT_LENGTH=16384 ollama serve` (see [.env.example](.env.example)), or
  - **gemini**: a `GOOGLE_API_KEY` from AI Studio

## Run

From this repo's root, against a fresh clone of catalog-poc (a git URL or a local path):

```bash
go run ./cmd/autodoc run --repo <catalog-poc git URL or path>
```

The target is cloned into the run's own directory, and the result comes back to it as one
commit on a new branch `autodoc/<base sha>`, so `git diff <base>..autodoc/<base sha>` shows
exactly what the run added. For a local path the branch is pushed into that repository on
every run: its working tree and checked-out branch are left alone. For a git URL it is pushed
only with `AUTODOC_GIT_TOKEN`.
With Gemini, for example:

```bash
AUTODOC_PROVIDER=gemini GOOGLE_API_KEY=... go run ./cmd/autodoc run --repo ../catalog-poc
```

Flags of `run`:

| Flag | Default | Meaning |
|---|---|---|
| `--repo` | (required) | git URL or local path of the service to document |
| `--main` | the one command that serves the routes | file with the `func main` to document, relative to the repo root |
| `--run-tests` | off | also run the target's own tests; reported, never blocks the run |
| `--runs-dir` | `runs` | where run directories go |

`go run ./cmd/autodoc smoke` (or `make smoke`) checks the model wiring with one tool call.
`make build` builds `bin/autodoc`; `make check` runs vet and the tests.

## Environment

Read from the environment, then from `.env` in the working directory (a shell export wins
over `.env`). Copy [.env.example](.env.example) to `.env` to start.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `AUTODOC_PROVIDER` | no | `local` | model backend: `local` or `gemini` |
| `GOOGLE_API_KEY` | with `gemini` | — | Gemini API key |
| `AUTODOC_LOCAL_URL` | no | `http://localhost:11434/v1` | OpenAI-compatible endpoint for `local` (Ollama, llama.cpp, LM Studio, vLLM) |
| `AUTODOC_LOCAL_API_KEY` | no | `local` (placeholder) | key for `local`, only if the server wants one |
| `AUTODOC_AGENT_MODEL` | no | local: `qwen3:14b`; gemini: `gemini-3.8-flash,gemini-3.7-flash,gemini-3.5-flash` | swagger and docs agents; comma-separated fallback chain, first choice first |
| `AUTODOC_JUDGE_MODEL` | no | local: `qwen3:14b`; gemini: `gemini-pro-latest,gemini-3.1-pro-preview,gemini-3.8-flash` | grounding judge; same format |
| `AUTODOC_HTTP_TIMEOUT` | no | `10m` for `local`, `90s` for `gemini` | timeout per HTTP attempt to the model, a Go duration such as `90s` |
| `AUTODOC_TOKEN_BUDGET` | no | `1000000` | tokens the whole run may use, all agents together; `0` for no limit |
| `AUTODOC_GIT_TOKEN` | no | — | when set, pushes the run branch to the https URL given as `--repo`; without it nothing leaves the machine (a local `--repo` gets the branch either way) |

## Outputs

Each run gets `runs/<run-id>/` (or under `--runs-dir`); the command prints its path.

- `trace.jsonl`: every model call, tool call, gate result and retry, as they happen
- `summary.json`: status, totals (calls, tokens), each step's outcome
- `baseline.json`, `facts.json`, `facts_annotated.json`, `gate_a.json`, `route.json`, `gate_b.json`, `judge.json`: per-step inputs and reports
- `work/`: the target's working copy. The result is committed there on branch
  `autodoc/<base sha>`, then pushed to the target repo (see Run): annotations in the handlers, the spec in `internal/apispec/`
  (`swagger.json`, `swagger.yaml`), the route serving it, and the pages in `docs/`
