# How to Run Cortex Evals

This runbook tells a future agent (or human) how to run, interpret, and score the two eval harnesses in this repo. It is the canonical entry point — if the commands here disagree with docstrings elsewhere, fix the docstrings to match this file.

There are two harnesses, and you should almost always run both:

1. **Fast eval** — `eval/run_eval.sh` — keyword-hit-rate gate over 100 questions. Cheap, deterministic, catches regressions in lexical recall.
2. **Deep eval** — `eval/deep/run_deep_eval.sh` — structured retrieval dump over 30 questions with rephrasings and negatives. Module-level correctness. **Scored by an agent**, not by a rubric.

Neither one tells you whether the retriever is good on its own. Run both, reconcile the disagreements, and read the deep-eval scored verdicts.

---

## Pre-flight

Before running *any* eval, confirm:

### Ingest freshness
```bash
./cortex ingest status --project=cortex
```
You want something like:
```
project=cortex entries=N modules=M last_commit=<sha> last_trail=... last_at=<iso>
```
If you see `RUN IN PROGRESS`, wait. If `last_commit` doesn't match `git rev-parse --short HEAD`, re-ingest:
```bash
go build -o cortex ./cmd/cortex
( nohup ./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) . > /tmp/cortex-ingest.log 2>&1 < /dev/null & )
```
Detached subshell (`( ... & )`) is load-bearing on macOS — plain `&` still ties the process to the TTY. `setsid` does not exist on macOS.

### Clean corpus
This is the subtle one. `.cortexignore` updates only affect what the walker *scans*; they do **not** retract previously-indexed entries. If you have reason to believe prior runs left stub entries (bodies like `"Module unknown:per-file:<path> (unknown)."` with nothing else) or polluting artifacts (e.g. old `eval/results*.json`, `.tasks/*`), you need a **clean rebuild** before the eval results will be interpretable:

```bash
./cortex down --purge        # wipes Neo4j + Weaviate derived state
./cortex up                  # boots the stack
# If Weaviate comes up without schema (known issue — cortex-afh/cortex-0u5), bootstrap manually:
#   (see internal/weaviate/CLAUDE.md for the HTTPClient.EnsureSchema path)
./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) .
```

The clean rebuild is **mandatory** after changes to `.cortexignore`, after ingest strategy changes, or after any summarizer regression — otherwise the old entries live forever.

### Stack health
```bash
./cortex status    # or read the hook's SessionStart output
```
Neo4j, Weaviate, Ollama must all be "healthy". If Ollama is timing out on large Go per-package summaries, check `Timeouts.IngestSummarySeconds` (default 1800s) and see beads cortex-8rk / cortex-ks1 for the known oversized-prompt failure mode.

---

## Fast eval — `eval/run_eval.sh`

### What it does
For each of 100 questions in `eval/questions.json`, runs `cortex recall <q> --limit 5 --json`, concatenates the result bodies, and computes `hits / len(expected_keywords)`. A question passes at `score >= 0.6`.

### How to run
```bash
JSON_OUT=eval/results.post-<tag>.json bash eval/run_eval.sh
```
- Use a distinct `JSON_OUT` per run so you keep the baseline for comparison. `eval/results.json` is a convenience alias; the baseline is `eval/results.baseline.json` — do not overwrite it.
- `LIMIT=5` is the default top-k per query. Override with `LIMIT=10 bash eval/run_eval.sh` if you are investigating a specific regression.
- Runtime: ~5-10 minutes depending on Ollama speed.

### How to interpret
- **PASS RATE** printed at the end. Compare to `eval/results.baseline.json` (15% at commit 556345a) and the most recent `eval/results.post-*.json` in the repo.
- **Blind spot:** this eval does not care *which* entry returned the keywords, only that *some* top-k body contains them. Large polluting modules (eval fixtures, `.tasks/cortex-phase1.task.json`) pass the gate with empty-bodied or placeholder entries because they contain every cortex concept. This is the cortex-6dx blind spot — a 92% fast-eval pass rate does **not** imply the retriever is accurate. Always also run the deep eval.
- **When to trust it:** pure regression detection. If fast eval drops from 92% to 60% after a change, that is a real regression. If it holds steady but deep eval drops, the deep eval is telling the truth.

---

## Deep eval — `eval/deep/run_deep_eval.sh`

### What it does
The Go runner (`eval/deep/runner/main.go`) executes every question in `eval/deep/questions_deep.json` (primary + rephrasings), captures full `cortex recall` output including scores, similarities, PPR, base activation, and bodies, and computes cheap deterministic metrics inline:
- P@1 / P@3 — expected module appears at rank 1 / in top-3
- MRR — reciprocal of the rank of the first expected module
- rephrasing_agreement — fraction of rephrasings whose top-1 module equals the primary's top-1 module
- negative_triggered — for `should_be_empty` questions, 1 iff every retrieval has `len == 0` or `top_score < neg_threshold` (0.05 default, matching `retrieval.forgetting.visibility_threshold`)

These are **advisory**. The real judgement is an agent pass over the dump (see scoring section below).

### How to run
```bash
go build -o cortex ./cmd/cortex
bash eval/deep/run_deep_eval.sh
# overrides:
K=10 NEG_THRESHOLD=0.03 bash eval/deep/run_deep_eval.sh
```
The runner writes `eval/deep/runs/<timestamp>.json` and prints a short summary to stderr. It does **not** commit files — you commit them explicitly after scoring.

Runtime: ~15-30 minutes. Each question hits Ollama for concept extraction + query embedding. Rephrasings multiply the count.

### Dump format
See `eval/deep/README.md` for the full schema. Key points:
- `header` records the commit, timestamp, and every knob. This is what makes old dumps re-scorable.
- `summary` carries the runner's advisory metrics. **Do not trust these without the agent pass.**
- `records[]` has per-question data including `retrievals[]` (one per query including each rephrasing), `hits[]` with full scoring breakdown and body text.

---

## Agent scoring pass (the critical step)

**This is where deep eval gets its teeth.** The runner metrics are advisory only. You — the agent reading the dump — produce the verdicts.

### What to write

Produce two files alongside the dump:

1. `eval/deep/runs/<timestamp>.scored.json` — conforms to `eval/deep/schema/scored.schema.json`. Structure:
   ```jsonc
   {
     "dump_file": "...",
     "dump_commit": "...",
     "scored_at": "<iso>",
     "scorer": {"kind": "agent", "name": "<model id + session context>"},
     "summary": {
       "pass": N, "partial": N, "fail": N, "skip": N,
       "p_at_1_raw": <from dump>,
       "p_at_3_raw": <from dump>,
       "mrr_raw": <from dump>,
       "rephrasing_agreement_raw": <from dump>,
       "negative_triggered_raw": <from dump>,
       "note": "<any corpus-pollution caveats>"
     },
     "verdicts": [
       {
         "id": 1,
         "intent": "concept|identifier|cross-module|negative",
         "verdict": "PASS|PARTIAL|FAIL|SKIP",
         "confidence": <0..1>,
         "failure_mode": "<tag>|null",
         "reasoning": "<one or two sentences>",
         "load_bearing": {"query": "<the query>", "rank": <int>}
       },
       ...
     ]
   }
   ```

2. `eval/deep/runs/<timestamp>.summary.md` — a short markdown digest intended to be diffed in PR review. Structure:
   - Header: commit, timestamp, totals
   - Root-cause call-out if corpus pollution is detected (see below)
   - Verdict table: `id | intent | verdict | failure_mode | one-line reason`
   - Regression vs. previous scored run: new FAILs, resolved FAILs, unchanged

### How to judge

For each record:

1. **Read `gold_answer`**. This is the ground truth.
2. **Walk `retrievals[0].hits` in rank order.** For rephrasings, cross-check that they surface the same body/module — low rephrasing agreement is a red flag regardless of top-1 correctness.
3. **Assign:**
   - **PASS** — the gold answer is fully recoverable from the retrieved bodies, and the right module is at rank 1 or 2.
   - **PARTIAL** — the retrieved bodies contain the concept but miss a load-bearing detail, OR the right module is buried at rank 3+, OR only some rephrasings find it, OR the right module is rank-1 but its body is a placeholder stub.
   - **FAIL** — the gold answer cannot be supported by any retrieved body, or the expected module is absent entirely from top-10, or a `should_be_empty` query returned hits with `top_score >= neg_threshold`.
   - **SKIP** — the record has `error` set, or the question itself is ambiguous. Do not guess.
4. **Cite the single most load-bearing hit in `load_bearing`.** One query, one rank.
5. **Keep `reasoning` to two sentences max.** Anything longer belongs in the surrounding markdown summary, not in the JSON.

### Corpus-pollution red flags

These are the patterns you will encounter when something is wrong with the *corpus*, not the retriever. If you see any of them, **say so in the `summary.note` field and call it out in the markdown root-cause section**. Do not mark everything FAIL without documenting the systemic cause, because the next reader needs to know whether to fix the retriever or rebuild the index.

1. **Placeholder stub bodies** — literal string `"Module unknown:per-file:<path> (unknown)."` with nothing else. These are from failed summarizer runs. The fix is a clean re-ingest (see Pre-flight), not retrieval tuning.
2. **Old eval artifacts** — `eval/results.baseline.json`, `eval/results.json`, `eval/questions.json` appearing at rank 1-3 across many queries. These are from before cortex-6dx; `.cortexignore` does not retract them retroactively. Fix is a clean re-ingest.
3. **Lexical magnets** — `.tasks/*.json` or similarly sized planning documents appearing at rank 1-3 for queries whose gold_answer is elsewhere. These are cortex-6dx-class pollution. Fix: add to `.cortexignore`, then clean re-ingest.
4. **Session observations at rank 1** — bodies that describe *this session's own work* ("cortex-XXX fix", "Bumped timeout...") with empty `module` field. These are observations you or another agent wrote via `./cortex observe` earlier in the session. Fresh base activation + moderate cosine similarity to almost anything = lexical attacker. Fix: demote observations in the composite or exclude them from eval dumps (follow-up: `CORTEX_EVALUATION_2026-04-13.md` R3).
5. **Negative queries never empty, composite ~0.3** — see cortex-7y4. Fix in progress (`RelevanceFloor` gate, needs re-tuning).

### Agent override of advisory metrics

The runner metrics can be wrong in both directions:

- **Runner says P@1=0, agent says PASS** — legitimate when the gold answer is recoverable from a non-expected module's body text. E.g., a spec answer recoverable from a code-review document. Flag in `reasoning`.
- **Runner says P@1=1, agent says FAIL or PARTIAL** — when the right module is returned but its body is a placeholder stub, the agent should downgrade. The runner cannot distinguish this; only a reader can.

Use your judgement. That's the point of this scoring pass.

---

## After the eval

### Record the findings
- Commit the dump + scored.json + summary.md. Never rewrite history on these files.
- **Write a `CORTEX_EVALUATION_<date>.md`** narrative report alongside the scored run. The narrative is what a human reviewing a PR actually reads. Include:
  - Executive summary with before/after metrics side-by-side
  - What the fixes actually do (code diff summary)
  - Why the results look the way they do (root cause analysis)
  - Remediation plan ranked by cost/value
  - Pass/partial/fail lists with one-line reasons
  - Beads touched or filed this session
- Reference the evaluation report in the cortex-NNN beads that drove the changes. Do **not** close beads that still need remediation — a fast eval passing is not the same as a deep eval passing.
- Save a cortex observation summarizing the key numbers and any corpus-pollution findings, using `./cortex observe "..." --kind=Observation --facets=domain:eval,project:cortex,subsystem:retrieval,kind-of-record:benchmark`. This is required by the `cortex-observation-nudge` hook and ensures future sessions can retrieve the baseline.

### Regression diff
When scoring a new run, always compare against the previous `<timestamp>.scored.json` in `eval/deep/runs/`. The summary markdown should include a regression section:
- **Resolved FAILs** — questions that FAILed last time and PASSed or PARTIALed this time.
- **New FAILs** — questions that PASSed last time and FAILed this time.
- **Unchanged FAILs** — same FAILs both times (investigate whether the failure mode is the same).

If new FAILs outnumber resolved FAILs, you have a regression. Do not blame the retriever without checking the corpus pollution red flags first.

### Re-run after remediation
After a clean re-ingest or a retrieval tuning change:
1. Rebuild the cortex binary: `go build -o cortex ./cmd/cortex`
2. Run the fast eval first (cheap sanity check).
3. If the fast eval is in the expected range, run the deep eval.
4. Re-score. Compare regression lists against the last scored run, not the baseline.
5. Update the evaluation report with the new section.

---

## Common pitfalls

1. **Assuming `.cortexignore` retroactively cleans the index.** It does not. You must re-ingest from an empty derived-state backend.
2. **Trusting the fast eval alone.** 92% pass rate with 0.04 P@1 is a real state — the fast eval passes because lexical-magnet modules contain every keyword, not because the retriever is correct.
3. **Running deep eval before ingest finishes.** Ingest takes 15-60+ minutes on a local Ollama. Check `./cortex ingest status` before starting the eval; `RUN IN PROGRESS` means wait.
4. **Forgetting to rebuild the cortex binary.** Config changes like timeout bumps require `go build -o cortex ./cmd/cortex` before they take effect in the running eval.
5. **Writing observations via `./cortex observe` during an eval session.** Those observations immediately enter the retrieval pool at full base activation and contaminate the subsequent deep-eval run. Batch your observations to the *end* of the session, after the eval is complete, or into a separate dedicated session.
6. **Amending baseline files.** `runs/baseline.json` and `runs/baseline.scored.json` are historical reference points. Don't delete or rewrite them. To capture a new baseline, run a new timestamped dump and reference both old and new in the PR description.
7. **Skipping the agent scoring pass.** The runner's advisory metrics are not verdicts. A run without `<timestamp>.scored.json` is data without interpretation.
8. **Interactive commands from the agent.** The deep eval scoring pass is a read-only interpretation task. Never modify the dump file, never re-score by re-running the dumper — if you disagree with the runner metrics, override them in `verdicts[]` and explain in `reasoning`, then commit the scored.json.

---

## Quick reference

```bash
# Fresh baseline from scratch (hours)
./cortex down --purge && ./cortex up
go build -o cortex ./cmd/cortex
./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) .
JSON_OUT=eval/results.post-<tag>.json bash eval/run_eval.sh
bash eval/deep/run_deep_eval.sh
# then: agent reads eval/deep/runs/<timestamp>.json and writes .scored.json + .summary.md + CORTEX_EVALUATION_<date>.md

# Incremental retest after a code change (minutes, use with caution — see pitfall #1)
go build -o cortex ./cmd/cortex
./cortex ingest --project=cortex --commit=$(git rev-parse --short HEAD) .
JSON_OUT=eval/results.post-<tag>.json bash eval/run_eval.sh
bash eval/deep/run_deep_eval.sh

# Score an existing dump without re-running it (no new data generated)
# Agent reads eval/deep/runs/<timestamp>.json and writes:
#   eval/deep/runs/<timestamp>.scored.json
#   eval/deep/runs/<timestamp>.summary.md
# per the scoring contract in eval/deep/README.md
```

---

## When to contact a human

- The cortex binary will not build. (Fix it, don't skip the eval.)
- Ollama is reporting `model not found` or authentication errors.
- The deep eval dump is empty (the Go runner reported 0 records). Check `header.runner` — the runner was probably looking at the wrong question file.
- You cannot reach a PASS/FAIL verdict because the `gold_answer` is ambiguous. Mark the record SKIP with a one-line note in `reasoning` explaining what's unclear, and flag it in the markdown summary. Do not guess.
- Total PASS rate is 0. Either the retriever is completely broken or the corpus is completely empty — either way the state needs human triage before further work.
