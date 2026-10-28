# INCREMENTAL — planned incremental runs

Not implemented. Today a run regenerates everything: `strip` removes every annotation, the docs step deletes every page and the judge starts with an empty cache, so a second run on the same repo asks the model for everything again. This is the plan to redo only what the code change touched.

## The idea

Each model-written **unit** — a handler's annotations, `main`'s general API info, a docs page, a judge verdict — gets a fingerprint of exactly the inputs it was produced from. A committed manifest maps fingerprint → output. The next run recomputes fingerprints from fresh facts: a match is reused, a mismatch is redone. Every deterministic gate still runs on everything, so reuse is never trusted blindly.

**Content fingerprints, not `git diff`:**
- a handler's annotations depend on types and constants in other files;
- lines added above a function change nothing that matters but show up in the diff;
- a new prompt or model makes old output stale even when the code is unchanged;
- the last output may sit on an unmerged `autodoc/<sha>` branch whose base is not an ancestor of HEAD.

`git diff` against the previous base is still used, only to report what changed.

## 1. Finding the previous run

- **Merged into HEAD:** the annotations, spec, pages and manifest are already in the clone.
- **Not merged:** `git clone` already fetches `origin/autodoc/*`. Take the branch whose manifest `base_sha` is the nearest ancestor of HEAD, else the newest; read its files with `repo.Show`.
- **None, or `--full`:** run exactly as today.

## 2. The manifest

`docs/.autodoc/manifest.json`, written only by committed runs, as the last step before `commit` (whose `allowed()` must accept it). It holds:
- **a key per agent:** hash of its instruction, tool declarations, model chain, the facts schema version and the swag version. A changed key makes all of that agent's units stale;
- **per unit:** input fingerprint, output hash and, for handlers, the annotation lines;
- **judge verdicts,** keyed by claim.

It lives in the target repo, not in `runs/`, so CI and other machines see it and the inputs travel with the output they produced.

## 3. Fingerprints

Line numbers are stripped everywhere (`file.go:42` → `file.go`, and from snippets), so code that only moved stales nothing.

| Unit | Inputs |
|---|---|
| Handler annotation | what `routeMessage` sends: route facts, related types and constants; comment-free hashes of the handler and of the functions in its `Calls`; replayed reads |
| `main` API info | `main`'s file, README.md, package docs; replayed reads |
| Docs page | `pageMessage(p)`: brief, mentions, facts sections, and the spec for api.md; replayed reads |
| Judge claim | the existing `cacheKey` without snippet line numbers, plus the judge's agent key |

**Replayed reads.** `read_file`, `get_facts` and `get_spec` are pure functions of the working copy. Each LLM session records `(tool, args) → result hash`; the next run makes the same calls, and any changed result stales the unit. This covers whatever the agent chose to read without per-tool rules; it needs a recording hook in `internal/tools`.

`gofacts` gains a comment-free token hash per function: like `guard.Fingerprint`, but per declaration and without positions.

## 4. Flow

```
fetch ─► baseline ─► index ─► PLAN ─► strip ─► REAPPLY ─► SWAGGER (stale only) ─► gate A (all) ─► add route ─► reindex ─► DOCS (stale only) ─► gate B (all) ─► JUDGE (new claims) ─► MANIFEST ─► commit
```

- **plan** (new, no model calls): marks each unit *reuse / update / new / removed* with the input that changed, in `plan.json`. `autodoc run --plan` stops here: a free preview.
- **strip:** unchanged and total, so gate A's code-unchanged proof stays simple.
- **reapply** (new, deterministic): writes the stored annotation lines back onto reused handlers through `patch_comment`'s code path. Removed routes simply don't get theirs back.
- **swagger:** only new or stale handlers, and `main` if stale; with nothing stale the agent is not built.
- **gate A,** on everything: an issue that maps (operation → route → handler) to a reused handler makes it stale; it gets a full annotate turn, then the usual fix loop.
- **docs:** reused pages are copied from the previous output; only stale pages get a session. A reused page flagged by gate B or the judge gets a new session with its brief, the current page and the issues (today's `Fix` assumes the session exists).
- **judge:** earlier verdicts are loaded into `j.verdicts` up front, so only new or changed claims are judged; earlier *unsupported* verdicts are judged again. Claims skipped over the budget in one run get judged in the next, so coverage grows across runs.

## 5. Update, not regenerate

A stale unit gets its previous output and what changed (the facts diff for its sections, the reads whose results differ) and is asked for the smallest edit, so diffs stay readable instead of rewording whole pages. Full rewrite when its agent's key changed or most of its inputs did.

## 6. Hand edits

An output that no longer matches the manifest's hash was edited by a person. It becomes the previous output — reused if its inputs are unchanged, updated if not, never overwritten — and is re-gated and flagged in `summary.json`.

## 7. Reporting

A `reuse` trace event per unit; reused / updated / regenerated counts with reasons in `summary.json`; the commit message lists what was regenerated and why. An unchanged service with a clean last run makes zero model calls and ends `unchanged`.

## 8. Tests

- Fingerprints: lines added above a handler change nothing; a JSON tag changed on a response type stales only the handlers using it, and api.md.
- Pipeline, with the `llmtest` fake model: a second run on `testdata/svc` makes zero calls; one edited handler in `orders.go` costs one swagger turn plus the affected pages.
- The previous run found on an unmerged branch; a changed agent key forcing a full run; a hand edit kept.

## 9. Rollout, one commit per stage

1. Function hashes, fingerprints, manifest written at commit — no change in behaviour.
2. Finding the previous run, `plan`, `plan.json`, `--full` — still regenerating everything, so the decisions are checked on catalog-poc before anything relies on them.
3. Cross-run judge cache: the most calls (up to 60 per pass) for the least risk.
4. Swagger reuse, reapply, the gate A fallback.
5. Docs reuse and read recording.
6. Update mode.
7. Hand edits.

## Trade-offs

- **Pages are coarse:** any route change stales api.md, a new file stales overview.md and structure.md. Update mode keeps those diffs small; per-section units could come later.
- **Update mode can carry stale prose forward:** the gates and the judge check facts, not fit. The full-rewrite cases limit it.
- **Reuse is re-gated:** a wrong reuse cannot pass gates A and B. Reused verdicts do skip re-judging, so they are keyed on the claim and its code and kept only from committed runs: a hand-edited manifest could at worst skip re-judging lines whose text and code are unchanged.
