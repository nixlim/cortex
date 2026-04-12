# Cortex Deep Eval

A data-capture harness for recall quality. The runner executes every question in `questions_deep.json`, dumps verbatim `cortex recall` output, and computes only cheap deterministic metrics. No LLM-judge, no Ollama in the loop. Subjective verdicts come from a separate scoring pass performed by an agent reading this README.

`eval/run_eval.sh` remains the fast lexical gate. Deep eval is opt-in — run it on demand before releases, after summarizer or retrieval changes, or when investigating a regression the fast gate did not catch.

## Layout

```
eval/deep/
├── README.md                  # this file — the scoring contract lives here
├── questions_deep.json        # question bank (see schema below)
├── run_deep_eval.sh           # wrapper over the Go runner
├── runner/main.go             # data collector (Go)
├── schema/
│   └── scored.schema.json     # JSON Schema for scoring output
└── runs/
    ├── <timestamp>.json              # dump — one per run (machine-written)
    ├── <timestamp>.scored.json       # verdicts — agent-written
    └── <timestamp>.summary.md        # markdown diff-friendly summary
```

## Question schema (`questions_deep.json`)

One JSON array. Each element:

| Field              | Type       | Notes                                                                                   |
| ------------------ | ---------- | --------------------------------------------------------------------------------------- |
| `id`               | integer    | Stable across runs. Do not reassign when editing.                                       |
| `q`                | string     | Primary question sent to `cortex recall`.                                               |
| `intent`           | string     | One of `concept`, `identifier`, `cross-module`, `negative`.                             |
| `expected_modules` | string[]   | Module paths that SHOULD appear in the top-k. Empty for `negative`.                     |
| `gold_answer`      | string     | Freeform prose the scorer judges retrieved bodies against.                              |
| `rephrasings`      | string[]   | Alternate phrasings of `q`. Each is run independently and its results captured.         |
| `should_be_empty`  | boolean    | `true` → query should return nothing (or only very low-score hits).                    |

**Minimums.** At least 30 questions total, with at least 5 per intent (`concept`, `identifier`, `cross-module`, `negative`) and at least 10 questions carrying two or more rephrasings.

### Module path granularity

A "module" is a file path, parsed from the result body with the regex `per-file:(\S+?)(\s|$|\))` (the leading fragment `Module <project>:per-file:<path> (...)` that ingest writes into every summary). Two retrieved chunks from the same file count as the **same module**. Matches for `expected_modules` and rephrasing-agreement are computed at this granularity — never at `EntryID`. Expected module strings must be the full repo-relative path, e.g. `internal/recall/CLAUDE.md`, not just `internal/recall`.

If the ingest strategy changes (e.g. from per-file to per-package), update the regex in `runner/main.go` and re-run a new baseline — do not attempt to compare across granularities.

## Runner contract (pinned defaults)

| Knob                      | Default | Flag / env                         | Meaning                                                                                   |
| ------------------------- | ------- | ---------------------------------- | ----------------------------------------------------------------------------------------- |
| Recall `--limit`          | 5       | `-k` / `K=`                        | Top-k captured per query.                                                                 |
| Negative trigger threshold| 0.05    | `-neg-threshold` / `NEG_THRESHOLD=`| Matches `retrieval.forgetting.visibility_threshold`. See below.                           |
| Per-query timeout         | 60s     | `-timeout` / `TIMEOUT=`            | Hard cap per `cortex recall` invocation.                                                  |
| Cortex binary             | ./cortex| `-cortex` / `CORTEX=`              |                                                                                           |

Every knob is recorded in `dump.header` so historical runs remain interpretable.

**Negative query semantics.** A question with `should_be_empty: true` is considered triggered (correctly empty) iff every retrieval (`q` plus each rephrasing) satisfies: `len(results) == 0` OR `results[0].score < neg_threshold`. Any single run that returns a high-scoring hit fails the negative check.

## Dump format (`runs/<timestamp>.json`)

```jsonc
{
  "header": {
    "timestamp": "20260412T140000Z",
    "commit": "556345a",
    "cortex_binary": "./cortex",
    "questions_file": "eval/deep/questions_deep.json",
    "recall_k": 5,
    "negative_score_threshold": 0.05,
    "module_regex": "per-file:(\\S+?)(?:\\s|$|\\))",
    "module_granularity": "file (per-file:<path>)",
    "runner": "eval/deep/runner (go)"
  },
  "summary": {
    "questions": 30,
    "scored_positive": 25,
    "p_at_1": 0.84,
    "p_at_3": 0.96,
    "mrr": 0.90,
    "rephrasing_agreement": 0.80,
    "negative_triggered": 1.00
  },
  "records": [
    {
      "id": 1,
      "q": "...",
      "intent": "concept",
      "expected_modules": ["internal/datom/CLAUDE.md"],
      "gold_answer": "...",
      "rephrasings": ["..."],
      "should_be_empty": false,
      "retrievals": [
        {
          "query": "...",
          "is_rephrasing": false,
          "hits": [
            {
              "rank": 1,
              "entry_id": "entry:01KNY...",
              "module": "internal/datom/CLAUDE.md",
              "score": 0.71,
              "base_activation": 0.12,
              "ppr_score": 0.33,
              "similarity": 0.78,
              "importance": 0.10,
              "why_surfaced": ["..."],
              "body": "Module cortex:per-file:internal/datom/CLAUDE.md ..."
            }
          ]
        }
      ],
      "metrics": {
        "p_at_1": 1,
        "p_at_3": 1,
        "mrr": 1,
        "rephrasing_agreement": 1
      }
    }
  ]
}
```

## Deterministic metrics (computed inline by the runner)

All metrics are computed on the **primary** `q` retrieval except `rephrasing_agreement`.

- **P@1** — 1 if any module in `expected_modules` appears in the primary retrieval's rank-1 hit, else 0.
- **P@3** — 1 if any module in `expected_modules` appears in the primary retrieval's top 3 hits, else 0.
- **MRR** — `1 / rank_of_first_expected_module` over the primary retrieval (0 if no expected module appears).
- **Rephrasing agreement** — fraction of rephrasings whose **top-1 module** equals the primary's top-1 module. Module-level, not chunk-level. Undefined (omitted) for questions with no rephrasings; 0 if the primary top-1 is empty.
- **Negative triggered** — boolean (only set for `should_be_empty` questions), per the rule above.

Run-level summary aggregates: P@1, P@3, MRR averaged over scored-positive questions (those with `!should_be_empty && len(expected_modules) > 0`); rephrasing_agreement over rephrasing-bearing positives; negative_triggered over negatives.

## Scoring contract (agent pass)

An agent reads `runs/<timestamp>.json` and produces two files in the same directory:

1. **`<timestamp>.scored.json`** — conforms to `eval/deep/schema/scored.schema.json`. Required fields:
   - `dump_file`, `dump_commit`, `scored_at`, `scorer.{kind,name}`
   - `verdicts[]` — one entry per record, keyed by `id`, with `verdict ∈ {PASS, PARTIAL, FAIL, SKIP}`, `confidence ∈ [0,1]`, `reasoning`, and (for FAIL/PARTIAL) a `failure_mode` tag
   - `load_bearing` — which `{query, rank}` from the dump the verdict relied on
   - `summary` — rollup counts

2. **`<timestamp>.summary.md`** — short markdown digest:
   - Header line: commit, timestamp, totals
   - Table: id / intent / verdict / failure_mode / one-line reason
   - Regression section: diff against the previous scored run (new FAILs, resolved FAILs, unchanged). This is what gets eyeballed in PRs.

### How to judge

For each question:

1. Read `gold_answer`.
2. Walk `retrievals[0].hits` in rank order; for rephrasings, cross-check that they surface the same body/module.
3. Assign:
   - **PASS** — the gold answer is fully recoverable from the retrieved bodies, and the right module is at rank 1 or 2.
   - **PARTIAL** — the retrieved bodies contain the concept but miss a load-bearing detail, or the right module is buried (rank 3+), or only some rephrasings find it.
   - **FAIL** — the gold answer cannot be supported by any retrieved body, or an expected module is absent entirely, or a `should_be_empty` query returned high-confidence hits.
   - **SKIP** — the dump has `error` set for this record, or the question itself is ambiguous; do not guess.
4. Cite the single most load-bearing hit in `load_bearing`. One hit, one query.
5. Keep `reasoning` to two sentences max.

Deterministic metrics are advisory. The agent MAY override them — e.g. a module-level P@1 of 0 can still earn a PASS if the retrieved body transparently contains the answer from a different file. Flag such cases in `reasoning`.

### Re-scoring old runs

Dump files are versioned. To re-judge a historical run with a newer model, run the scoring pass against the old `<timestamp>.json` and write `<timestamp>.scored.v2.json` alongside it. Do not overwrite prior scored files.

## Running

```bash
go build -o cortex ./cmd/cortex
bash eval/deep/run_deep_eval.sh
# or with overrides:
K=10 NEG_THRESHOLD=0.03 bash eval/deep/run_deep_eval.sh
```

The runner writes `eval/deep/runs/<timestamp>.json` and prints a short summary to stderr. It does not commit files — commit the dump (and baseline scored.json) explicitly after reviewing.

## Baseline

`runs/baseline.json` and `runs/baseline.scored.json` are committed reference points. Do not delete them. When the retrieval pipeline changes materially, capture a new baseline, leave the old one in place, and reference both in the PR description so regression analysis can diff against either.
