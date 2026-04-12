# Deep eval baseline — commit 556345a (2026-04-12)

**Run:** `runs/baseline.json` · **Scored by:** claude-opus-4-6 · **k:** 5 · **neg_threshold:** 0.05

## Totals

| Metric             | Value |
| ------------------ | ----- |
| Questions          | 30    |
| PASS               | 4     |
| PARTIAL            | 12    |
| FAIL               | 14    |
| P@1 (module)       | 0.16  |
| P@3 (module)       | 0.36  |
| MRR                | 0.30  |
| Rephrasing agree   | 0.24  |
| Negative triggered | 0.00  |

## Verdicts

| id | intent        | verdict | failure_mode        | one-line reason                                                                 |
| -- | ------------- | ------- | ------------------- | ------------------------------------------------------------------------------- |
|  1 | concept       | PASS    | —                   | rank-1 internal/datom/CLAUDE.md; rank-2 cortex-spec.md                           |
|  2 | concept       | PARTIAL | rank_too_low        | cortex-spec.md at rank 3; activation module absent                              |
|  3 | concept       | PARTIAL | rank_too_low        | cortex-spec.md at rank 2 under eval self-hit                                    |
|  4 | concept       | FAIL    | over_retrieval      | top-3 all eval/*; recall package absent                                         |
|  5 | concept       | PASS    | —                   | cortex-spec.md at rank 1 covers tail validation                                 |
|  6 | concept       | PARTIAL | rank_too_low        | cortex-spec.md at rank 3; activation module absent                              |
|  7 | concept       | FAIL    | over_retrieval      | internal/trail absent; rank-1 eval artifact                                     |
|  8 | concept       | PARTIAL | rank_too_low        | cortex-spec.md at rank 3 under eval self-hits                                   |
|  9 | concept       | FAIL    | over_retrieval      | internal/reflect + cortex-spec.md absent                                        |
| 10 | concept       | PASS    | —                   | cortex-spec.md at rank 1 covers self-heal                                       |
| 11 | identifier    | PARTIAL | rank_too_low        | cortex-spec.md at rank 3; internal/config absent                                |
| 12 | identifier    | PASS    | —                   | cortex-spec.md at rank 1 lists the constant                                     |
| 13 | identifier    | FAIL    | under_retrieval     | cmd/cortex absent; buildRecallPipeline not recoverable                          |
| 14 | identifier    | FAIL    | under_retrieval     | internal/recall absent; recall.Response not recoverable                         |
| 15 | identifier    | PARTIAL | wrong_module        | research draft at rank 2; cortex-spec.md absent                                 |
| 16 | identifier    | FAIL    | under_retrieval     | top-3 all eval/*; retract docs absent                                           |
| 17 | identifier    | FAIL    | under_retrieval     | top-3 all eval/*; visibility_threshold key absent                               |
| 18 | identifier    | PARTIAL | wrong_module        | research draft at rank 3; cortex-spec.md absent                                 |
| 19 | cross-module  | FAIL    | under_retrieval     | neither internal/recall nor cmd/cortex in top-5                                 |
| 20 | cross-module  | FAIL    | wrong_module        | research draft at rank 1; canonical modules absent                              |
| 21 | cross-module  | PARTIAL | under_retrieval     | cortex-spec.md at rank 2; 3 target packages absent                              |
| 22 | cross-module  | PARTIAL | incomplete_answer   | cortex-spec.md at rank 1; internal/errs + cmd/cortex absent                     |
| 23 | cross-module  | PARTIAL | wrong_module        | cortex-spec.md at rank 3 under research draft                                    |
| 24 | cross-module  | PARTIAL | under_retrieval     | cortex-spec.md at rank 3; cmd + internal packages absent                        |
| 25 | cross-module  | PARTIAL | incomplete_answer   | cortex-spec.md at rank 1; ingest/log/replay packages absent                     |
| 26 | negative      | FAIL    | over_retrieval      | rank-1 score 0.34 >> 0.05 threshold                                             |
| 27 | negative      | FAIL    | over_retrieval      | high-score hits on eval artifacts                                                |
| 28 | negative      | FAIL    | over_retrieval      | high-score hits on eval artifacts                                                |
| 29 | negative      | FAIL    | over_retrieval      | high-score hits on eval artifacts                                                |
| 30 | negative      | FAIL    | over_retrieval      | high-score hits on eval artifacts                                                |

## Regression section

Baseline run — no prior run to diff against. Future runs should compare verdict deltas per `id` against `baseline.scored.json`: new FAILs are regressions, new PASSes are wins, `PARTIAL → FAIL` is a soft regression worth flagging.

## Key findings the fast gate missed

1. **Eval self-recall.** `eval/results*.json`, `eval/questions.json`, `eval/run_eval.sh` dominate rank-1 for 21 of 30 questions. These files were ingested and their verbatim keyword lists act as lexical magnets. Fix: add them to `.cortexignore`, rebuild, re-baseline.
2. **Negative queries never trigger empty (0/5).** GraphQL, OAuth, React, Kafka, billing — none exist in cortex. Every one returned eval artifacts at score 0.3+. The composite activation score has no calibrated "no relevance" floor; investigate a minimum similarity gate.
3. **Cross-module questions 0/7 PASS, 5/7 PARTIAL, 2/7 FAIL.** Expected: multi-module questions need multi-module retrieval. PPR seeds from a single concept extractor may be under-diversifying; investigate multi-seed expansion.
4. **Identifier questions over-rely on `docs/spec/cortex-spec.md`.** When a config key is asked for, `internal/config/CLAUDE.md` never surfaces ahead of the spec — the spec is winning because it mentions every key. Not a bug per se, but means the per-package CLAUDE.md summaries may be underweighted.

File each as its own bead; do NOT tune retrieval to hide these — the baseline is the record that these were the starting point.
