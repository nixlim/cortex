# Cortex Evaluation Report — 2026-04-13

**Commit evaluated:** `4d34969`
**Session focus:** verification of cortex-6dx (eval self-recall) and cortex-7y4 (negative-query relevance floor)
**Agent:** claude-opus-4-6 (1M context)

## Executive summary

The two target fixes shipped and the unit tests pass, but the end-to-end verification surfaced **four separate DB-hygiene problems** that had been quietly masking each other. The fast eval improved dramatically. The deep eval regressed. That split is the single most important finding of the session: the fast eval (`eval/run_eval.sh`) measures keyword presence in body text and is largely invariant to which specific entry is returned, while the deep eval (`eval/deep/run_deep_eval.sh`) measures module-level correctness and rewards substantive summaries. They disagree because the underlying corpus is deeply polluted in ways the fast eval does not detect.

| Metric | Pre-fix baseline (556345a) | This run (4d34969) | Δ |
|---|---|---|---|
| **Fast eval pass rate** | 15 / 100 (15%) | **92 / 100 (92%)** | +77pp |
| **Deep eval P@1** | 0.16 | 0.04 | −0.12 |
| **Deep eval P@3** | 0.36 | 0.32 | −0.04 |
| **Deep eval MRR** | 0.30 | 0.21 | −0.09 |
| **Deep eval negative_triggered** | 0.00 (0/5) | **0.00 (0/5)** | — |
| **Deep eval agent verdict** | 4 PASS / 12 PARTIAL / 14 FAIL | 2 PASS / 3 PARTIAL / 25 FAIL | worse |

## What the fixes actually do

### cortex-6dx — `.cortexignore` excludes eval artifacts
Added to `.cortexignore`:
```
eval/results.json
eval/results.baseline.json
eval/results.*.json
eval/questions.json
eval/run_eval.sh
eval/deep/questions_deep.json
eval/deep/run_deep_eval.sh
eval/deep/runs/
```
Confirmed effect: `cortex walker` now skips these paths on new ingest runs.
Unconfirmed effect: **the old entries for these files are still in Neo4j/Weaviate from before the fix.** `.cortexignore` is a walker-side gate; it does not emit retractions for entries the previous ingest wrote. The only way to purge them is a clean re-ingest (see remediation below).

### cortex-7y4 — `recall.Pipeline.RelevanceFloor`
New field on `recall.Pipeline`. In the scoring loop, a candidate is dropped before the composite is computed when `max(similarity, ppr) < RelevanceFloor`. Zero disables. Wired through `retrieval.relevance_floor` in config with a default of `0.10`. Tests added:
- `TestRecall_RelevanceFloorDropsIrrelevantCandidates` — both sim and ppr well below 0.10 → empty result.
- `TestRecall_RelevanceFloorKeepsOnRampCandidates` — either signal above threshold → surface.
Both pass. **But** measurement at recall time shows the 0.10 default is well below the empirical cosine similarity floor for unrelated cortex text pairs, so the gate does not actually fire on realistic queries. See Root-cause #4 below.

### Peripheral changes
- `Timeouts.IngestSummarySeconds` bumped from 600s to 1800s (`internal/config/defaults.go`) after observing 600s deadlines on large Go per-package summarizer runs.
- Bead `cortex-ks1` (P1) filed for the root cause: even 1800s is insufficient for the `cmd/cortex` package because the prompt exceeds `num_ctx`. Needs size-aware fallback to per-file summaries.
- Bead `cortex-39m` (P2, blocked by ks1) filed for AST-aware chunking follow-up.
- Bead `cortex-1b7` (P1) filed separately for the `analyze/reflect returns 0 candidates` chicken-and-egg with community nodes (unrelated to this session, surfaced in a parallel bug report).
- Walker's `.cortexignore` also updated to exclude `.tasks/` (not in the cortex-6dx original scope — caught during post-mortem of the deep eval dump).

## Why the fast eval passed at 92%

The fast eval scores each question by a keyword-hit rate over the top-5 result bodies. A question like "what does cortex status show" (id=93) passes as long as **some** surfaced body contains the keywords `modules`, `last_commit`, `trail`, etc. The eval does not care *which* entry surfaced those keywords. Large polluting modules like `.tasks/cortex-phase1.task.json` and `eval/results.baseline.json` contain practically every cortex concept in their bodies, so they satisfy the keyword gate for most positive questions even when they are not the right answer. This is cortex-6dx's original root cause — the keyword gate has the same blind spot that made the eval fixtures become lexical magnets in the first place.

Net: the 92% figure is real in the sense that the gate passes, but it does not demonstrate that the retriever is returning the correct module. The deep eval does demonstrate that, and the verdict is much worse.

## Why the deep eval regressed

Four independent issues, each caught by the agent-scoring pass by walking individual retrievals:

### 1. Failed summarizer runs left placeholder stub entries
Most `*/CLAUDE.md` and `docs/spec/*.md` entries have bodies that read literally `"Module unknown:per-file:<path> (unknown)."` with **no actual summary**. The module name is correct; the content is empty. Evidence:

- Rank-1 for id=1 is `internal/datom/CLAUDE.md`, body = `"Module unknown:per-file:internal/datom/CLAUDE.md (unknown)."` — one line, no details.
- Rank-1 for id=3 is `internal/activation/CLAUDE.md`, same one-line stub.
- Rank-1 for id=6/7/14/16/19/24/27/29/30 is `cmd/cortex-mcp/CLAUDE.md` stub.
- Rank-1 for id=13/25 is `docs/spec/cortex-spec.md` stub.

The only non-stub body in the entire top-k of any question is `internal/analyze` (id=23), which ingested successfully as a Go per-package summary earlier this session after the timeout bump. That is also the only PASS verdict.

These stubs were written by some earlier ingest run whose summarizer failed silently — the walker created an entry and the summarizer timed out or errored, but the entry was not retracted. The incremental ingest at commit 4d34969 classifies these modules as "already ingested" and skips them, so the stubs live forever until an explicit rebuild.

### 2. Pre-cortex-6dx eval artifacts were not evicted
`eval/results.baseline.json`, `eval/results.json`, and `.tasks/cortex-phase1.task.json` are still in the index. `.cortexignore` only affects what the walker *scans*; it does not retract existing entries. These three files appear at rank 1-3 for 21/30 deep-eval questions, exactly matching the pre-fix baseline pattern described in cortex-6dx. The fix was never applied to the live DB — only to the walker rules. This is the single biggest deep-eval regression cause.

### 3. Session observations became lexical attackers
During the cortex-7y4 fix I wrote three cortex observations (via `./cortex observe`) describing the fix itself: the timeout bump, the RelevanceFloor gate design, and the ACT-R base-activation floor gotcha. Each is a ~500-char text blob with concepts like `retrieval`, `recall`, `pipeline`, `ingest`, `composite`, `relevance`, `similarity`. Their base activation is 1.0 (just-written) and their cosine similarity to most cortex queries is 0.5+ (topic overlap). With `w_base * B(e) = 0.3` contributing to every fresh entry, these observations dominate rank-1 for id=2, 11, 20, 21, 26, 28 — and they have no module path, so they appear in the dump with an empty `module` field.

This is an instance of the structural ACT-R gotcha I saved to cortex in this same session: **the composite has a 0.3 floor for any fresh entry** regardless of relevance. The gate added in cortex-7y4 was supposed to address this but is set too permissively (see #4).

### 4. RelevanceFloor=0.10 is too low for the empirical distribution
The cortex-7y4 gate requires `max(similarity, ppr) >= 0.10`. Measurements from this run's dump:

| Query type | sim (rank 1) | ppr (rank 1) | gate triggers? |
|---|---|---|---|
| Relevant concept (id=1) | 0.69 | 0.15 | no (correctly, want it to pass) |
| Irrelevant positive (session obs @ id=2) | 0.54 | 0.00 (empty module) | no (wrongly, want it to drop) |
| Negative (id=26, graphql) | 0.59 | 0.15 | no (wrongly, want it to drop) |
| Negative (id=30, billing) | 0.60 | 0.15 | no (wrongly, want it to drop) |

The cosine similarity between any two unrelated cortex-ish texts using `nomic-embed-text` is typically 0.4-0.6 because the embeddings share the same domain-vocabulary mean. A floor of 0.10 cannot distinguish "on-topic real answer" from "off-topic placeholder". An effective floor would need to sit around `0.70` or higher, which would also kill some legitimate low-sim results that currently pass because their PPR is strong.

The right fix is probably not "raise the floor" but one of:
- Gate on `w_sim*sim + w_ppr*ppr >= threshold` (composite minus the base contribution), so the base-activation floor cannot mask absent query signal.
- Normalize similarity against a calibrated "expected cosine between this query and an average entry", so the gate is "is this materially above random?".
- Subtract a global mean similarity baseline at query time.

## Remediation plan (ranked by cost/value)

The work below restores the deep eval to something meaningful. Tasks are named but not filed as beads yet — they are follow-up candidates.

### R1 — Full re-ingest after `cortex down --purge` (hours, mandatory)
The only path that actually removes the stubs + eval artifacts is to wipe the derived backends (`cortex down --purge`), rebuild the Weaviate schema (known missing step, workaround described in this project's memory), and re-ingest from scratch. Until this is done the deep eval is measuring a polluted corpus and any further retrieval tuning flies blind.

### R2 — Retry policy on summarizer failures (ks1 dependent)
Even with R1, the `cmd/cortex` Go per-package summarizer still fails at 1800s. Until cortex-ks1 lands (size-aware fallback to per-file), the ingest will leave one or two stub entries per run. Workarounds:
- Mark failing modules as "not indexed" rather than writing an empty-body placeholder. The current behavior is the worst of both worlds.
- Optionally: on `cortex ingest`, retract any module whose prior run emitted a SUMMARIZER_FAILED error so it is excluded until cortex-ks1 lands.

### R3 — Don't write session observations during eval-sensitive sessions
Observations from `./cortex observe` go into the same retrieval pool as ingest content and compete with it at full base activation. This session wrote three observations (correctly, per the `cortex-observation-nudge` hook), each of which became a rank-1 attacker. Options:
- Tag observations with `kind-of-record:session-note` or similar and demote them in the composite (e.g. multiply their `imp` by −0.2 so they naturally rank lower unless the query is about the session itself).
- Give observations a smaller initial `base_activation` than ingest entries (e.g. 0.3 instead of 1.0) so they start below the deep-eval threshold and only surface when relevance is high.
- Or: keep them at full activation but exclude them from the deep eval runner by module-path filter (observations have empty module paths in the current dumper).

### R4 — Re-calibrate `RelevanceFloor` from measured similarity
Run `cortex recall` over the deep-eval question bank, collect the similarity distribution for rank-1 hits on (positive, negative) questions separately, and set the floor at the point where the two distributions diverge. This is a one-shot tuning exercise that should land together with the gate-formulation change (#4 under "Why the deep eval regressed").

### R5 — Deep-eval harness rerun
After R1-R4, re-run `bash eval/deep/run_deep_eval.sh`. Target thresholds (from the eval/deep/README.md contract):
- P@1 ≥ 0.60 (vs. today's 0.04)
- P@3 ≥ 0.80 (vs. 0.32)
- MRR ≥ 0.65 (vs. 0.21)
- `negative_triggered` ≥ 0.80 (vs. 0.00)

## Pass list

Only one question PASSed cleanly:

- **id=23 (cross-module):** "how does PPR get its seeds from concept extraction output". Rank-1 was `internal/analyze` (Go per-package summary, non-stub body). Gold answer is not fully recoverable from that specific body, but the rank is stable across rephrasings and the body is substantive. Agent awarded PASS because this is the one retrieval in the dump that demonstrates the infrastructure works *when the underlying entry has a real body*. Every other PASS-candidate was blocked by one of the four root causes above.

## Partial list (3)

- **id=1 (concept):** right module rank-1, empty stub body. Name-level correct, content-level empty.
- **id=3 (concept):** `internal/activation/CLAUDE.md` rank-1, same stub.
- **id=5 (concept):** `docs/spec/cortex-spec.md` rank-2, same stub.
- **id=8 (concept):** `docs/spec/cortex-spec-code-review.md` rank-1 with a real body (this document was ingested, per the deep dump) that mentions storage backends at the audit level. A careful reader could extract (Neo4j, Weaviate, Ollama) from it.
- **id=15/17 (identifier):** partial credit to session observations that happened to mention config defaults in passing.
- **id=18 (identifier):** weak partial, stub-body doc at rank 3.
- **id=25 (cross-module):** stub at rank-1, right module.

(The JSON scored file lists 3 PARTIAL in the summary counter; the narrative above enumerates more than 3 because some are weak partials where the agent-confidence is under 0.7. Reconcile during next scoring pass.)

## Fail list (25)

See the verdict table in `eval/deep/runs/20260412T225007Z.summary.md`. Failure modes, by frequency:
- `wrong_module_stub` — 7
- `session_observation_pollution` — 6
- `lexical_magnet_pollution` — 5 (all `.tasks/cortex-phase1.task.json`)
- `stub_body` — 4
- `negative_not_empty` — 5 (all negative queries)
- `partial_session_observation` — 2 (PARTIAL)

## Operational notes

- The deep-eval runner's `module_regex` (`per-file:(\S+?)(\s|$|\))`) correctly parses `internal/datom/CLAUDE.md` style paths. The "empty module" hits in the dump (id=2, 11, 20, 21, 26, 28 rank-1) are genuine — they are session observations written via `cortex observe` that carry no `per-file:` prefix in their body.
- The `cortex ingest status` command shows cumulative counts (modules: 118, entries: 116) but does not distinguish stub entries from real ones. Consider adding a `--verify-bodies` flag that flags placeholder-only entries. Not filed.
- The `watch ./cortex ingest status` command will occasionally show truncated output mid-render if the status line is being rewritten as the walker loop advances. Not a bug; the status is consistent between redraws.

## Artifacts

- `eval/results.post-7y4.json` — fast eval JSON (92/8/0).
- `eval/deep/runs/20260412T225007Z.json` — deep eval dump.
- `eval/deep/runs/20260412T225007Z.scored.json` — agent verdicts.
- `eval/deep/runs/20260412T225007Z.summary.md` — agent summary.
- This file — full narrative.
- `HOW_TO_RUN_EVALS.md` — step-by-step runbook for future agents.

## Beads touched or filed this session

- `cortex-6dx` — CLAIMED (implemented `.cortexignore` updates; re-ingest incomplete). Do NOT close until R1 above lands.
- `cortex-7y4` — CLAIMED (implemented RelevanceFloor gate; default value needs re-tuning per R4). Do NOT close until R4 lands.
- `cortex-8rk` — timeout default bumped 600s → 1800s (test updated).
- `cortex-ks1` — NEW (P1). Size-gate per-package modules with per-file fallback.
- `cortex-39m` — NEW (P2, blocked by ks1). AST-aware chunking.
- `cortex-1b7` — NEW (P1). analyze/reflect never produce candidates (community nodes never written — filed from `/tmp/cortex-insights/bug-report.md`).
