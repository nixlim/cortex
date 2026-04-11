---
name: cortex-eval
description: Run the 100-question recall eval against cortex's current index to catch recall regressions. Use when the user asks "run the eval", "test cortex recall", "evaluate recall quality", after changes to the write/recall/ingest pipelines, after tuning activation/visibility/PPR parameters, or when investigating whether a recall change helped or hurt. The harness is a black-box semantic eval: it runs `cortex recall` against 100 factual codebase questions and scores top-5 results by keyword hit-rate.
---

# cortex-eval — the 100-question recall harness

A regression eval for `cortex recall` lives at `eval/` in the repo. It fires
100 factual questions at the current index and scores the top-5 results by
keyword-hit rate. Use it to catch recall regressions after touching any of:

- `internal/recall/*` — pipeline stages, rerank math
- `internal/write/*` — concept extraction, link derivation, embedding
- `internal/ingest/*` — module summary prompt, walker
- `cmd/cortex/recall.go` / `cmd/cortex/ingest.go` — wiring and config plumbing
- `internal/config/defaults.go` — activation / forgetting / PPR defaults

## Files

| Path | Role |
|---|---|
| `eval/run_eval.sh` | Bash harness — iterates questions, calls `cortex recall --json`, scores |
| `eval/questions.json` | 100 Q&A pairs: `{id, q, kw}`. `kw` is the list of expected keywords |
| `eval/results.json` | Per-question verdicts + summary. Rewritten on every run |

## Running the eval

Stack must be up and the current build must be installed at `./cortex`:

```bash
./cortex up                        # ensure neo4j/weaviate/ollama are healthy
go build -o cortex ./cmd/cortex    # rebuild if code changed
./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) .   # re-ingest
bash eval/run_eval.sh              # fire all 100 queries
```

The harness prints a line per question (PASS / FAIL / EMPTY, score, missed
keywords), then a summary, and writes `eval/results.json`.

### Knobs

```bash
LIMIT=10 bash eval/run_eval.sh                    # top-10 instead of top-5
JSON_OUT=eval/results-after-fix.json bash eval/run_eval.sh   # rename output
```

## Re-ingesting cleanly

The ingest state store at `~/.cortex/state/ingest/cortex.json` remembers
previously-ingested modules and will skip them. If you're testing a change
to the write/ingest pipeline, **delete the state file first** or your
re-ingest will be a no-op:

```bash
rm ~/.cortex/state/ingest/cortex.json
./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) .
```

## Scoring rubric

For each question the harness concatenates the bodies of the top-K results,
lowercases, and counts how many of the question's expected keywords appear
anywhere in that combined text (case-insensitive substring match).

| Verdict | Meaning |
|---|---|
| **PASS**  | score ≥ 0.60 (3+ of 5 keywords hit) |
| **FAIL**  | 0 < score < 0.60 (relevant retrieval, incomplete keyword coverage) |
| **EMPTY** | zero results returned from `cortex recall` — pipeline failure |

**What to watch for**:

- **Rising `EMPTY` count** — a recall *pipeline* regression. Something broke
  concept extraction, seed resolution, PPR, or entry loading. Check the
  Neo4j concept graph: `MATCH (e:Entry)-[:MENTIONS]->(:Concept) RETURN count(*)`.
- **Stable low `PASS`, high partial-match FAIL** — recall is working, but
  ingest summaries don't quote the keywords the questions ask about. This is
  a *content* problem, not a recall problem (see `cortex-v41`).
- **Dropping PASS after a ranking change** — the ACT-R rerank math, decay
  exponent, or weights shifted. Compare `eval/results.json` diffs on the
  per-question `score` fields.

## Interpreting `results.json`

```json
{
  "summary": { "pass": 9, "fail": 89, "empty": 2, "total": 100, "pass_rate": "9.0%" },
  "questions": [
    {
      "id": 1,
      "q": "what is a datom and what fields does it contain",
      "verdict": "FAIL",
      "score": 0.40,
      "matched_kw": "datom,actor",
      "missed_kw": "tx,checksum,op",
      "top_bodies": ["...first three result bodies..."]
    }
  ]
}
```

Useful one-liners:

```bash
# Score distribution
jq '[.questions[] | select(.score >= 0.4)] | length' eval/results.json
jq '[.questions[] | select(.score == 0)] | length'   eval/results.json

# Questions where recall returned something but keywords missed
jq '.questions[] | select(.verdict=="FAIL" and .score>0) | {id,q,score,missed_kw}' eval/results.json

# Questions where recall returned nothing (pipeline failures)
jq '.questions[] | select(.verdict=="EMPTY") | {id,q}' eval/results.json

# Diff two runs (before/after a fix)
diff <(jq -r '.questions[] | "\(.id)\t\(.score)"' eval/results-before.json) \
     <(jq -r '.questions[] | "\(.id)\t\(.score)"' eval/results-after.json)
```

## Known baseline (as of commit 62b59f8)

After fixing `cortex-v4g` (ingest Neo4j wiring) and `cortex-upp` (recall
config wiring + visibility_threshold default):

```
9 PASS  (≥0.60)
36 ≥0.40
66 ≥0.20  (semantic-hit on ≥1 keyword)
33 zero-hit
2 EMPTY
```

Regressions from this baseline mean something broke. Improvements above it
(especially landing `cortex-v41`) are the point of running the harness.

## Editing the question set

`eval/questions.json` is hand-authored. Each entry:

```json
{"id": 42, "q": "what is the decay exponent default value", "kw": ["decay","exponent","0.5","activation","base"]}
```

Rules:
- `q` is what goes to `cortex recall` verbatim
- `kw` is 4-6 substrings the *ideal* answer body would contain (lowercase
  match, so capitalization in `kw` doesn't matter)
- Prefer concrete tokens: constant values (`0.85`, `0.05`), identifier names
  (`leiden`, `nomic`, `ulid`), domain terms (`fsync`, `flock`, `ppr`)
- Avoid stopwords and generic English as keywords — they'll match any body
  and inflate scores falsely

## Writing an observation when you run the eval

After a meaningful run (baseline, post-fix, post-tuning), record the result:

```bash
./cortex observe "100Q recall eval at <commit-sha>: <pass> PASS / <fail> FAIL / <empty> EMPTY; <change summary>" \
  --kind=Observation \
  --facets=domain:eval,project:cortex,subsystem:recall,kind-of-record:benchmark
```

This gives your future self (and the next session) a baseline to compare
against without re-running the whole harness.

## Don'ts

- **Don't edit `eval/results.json` by hand** — it's rewritten on every run.
  Rename it (`JSON_OUT=eval/results-foo.json`) if you want to keep one.
- **Don't change the scoring rubric to "make the number go up"** — the whole
  point is stability across runs. If the keyword set for a question is
  genuinely wrong, fix `questions.json`, not `run_eval.sh`.
- **Don't run the eval without re-ingesting if the write pipeline changed**
  — you'll be measuring stale entries that don't reflect your fix.
- **Don't ignore EMPTY results** — even one is a red flag that something in
  the recall pipeline is silently failing. Run a single `cortex recall` on
  the EMPTY question with `--json` and trace through concept extraction →
  seeds → PPR → loader manually.
