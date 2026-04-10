---
name: cortex-guide
description: How to use Cortex (the local knowledge substrate this project ships) to persist and recall agent-grade knowledge across sessions. Use when the user mentions cortex, observe, recall, trails, "remember this", "what do we know about X", or is about to make a design decision worth preserving. Also consult at the start of any non-trivial task to surface prior observations.
---

# Cortex — persistent memory for this project

Cortex is the binary this repo ships. It stores observations, decisions, and
discoveries as an append-only datom log and exposes them through `cortex
recall`. It is **local only** (no cloud), runs against a Docker-managed
Weaviate + Neo4j stack, and uses a host-local Ollama for embeddings and LLM
prompts. Every write lands in `~/.cortex/log.d/` first; the backends are
rebuildable derived state.

Use cortex when the value of a piece of information outlives a single
session: design decisions, tricky bugs, cross-project patterns, benchmarks
results, anything a future agent (you, in a new chat) would want to find.

---

## Golden-path commands

| Command | Purpose |
|---|---|
| `cortex recall "<query>"` | Default-mode retrieval: concept extract → PPR over semantic graph → ACT-R rerank. This is how you ask "what do we already know about X?". |
| `cortex observe "<body>" --kind=<kind> --facets=<k:v,...>` | Record a single episodic entry. Every successful observe prints an `entry:<ulid>`. |
| `cortex trail begin --agent=<name> --name=<label>` | Start a work-session envelope. Prints a `trail:<ulid>`. |
| `cortex trail end` | Close the active trail (from `CORTEX_TRAIL_ID`) and generate an LLM summary. |
| `cortex history <id>` | Show the full retract-aware lineage of an entity. |
| `cortex status` / `cortex doctor` | Health and readiness of the managed stack. |

Run `cortex help <command>` for the full flag set. `docs/cli-reference.md`
has an exhaustive reference.

---

## When to reach for cortex

### Before a non-trivial task — RECALL first

If the user asks you to fix a bug, implement a feature, or understand an
unfamiliar subsystem, run `cortex recall` with the most specific phrase you
can extract from the request. A warm cortex will surface past decisions and
observations that change your approach.

```bash
cortex recall "retry strategy" --limit=5
cortex recall "weaviate schema drift" --json    # JSON for programmatic parsing
```

The result carries a `WhySurfaced` field explaining the score breakdown
(base activation, PPR, similarity, importance). Hits with a visible
`TrailContext` or `CommunityContext` come from episodic bundles — read
those in full; they often contain the "why" behind the "what".

If `results=0`, that's informative too: the problem is new, and your task
produces the first observations.

### During the task — OBSERVE the valuable bits

An observation is worth writing when it answers "a future me would have
wanted to know this":

- Root causes (not symptoms) of bugs you just fixed.
- Architectural decisions with trade-offs: "chose X over Y because Z".
- Performance envelopes you measured.
- Cross-cutting invariants you discovered while reading code.
- Surprising library/tool behaviour.

**Do not** observe:

- Every file you touched (the commit log has that).
- Step-by-step task progress (the conversation already has that).
- Transient in-conversation state (use a plan or tasks instead).

```bash
cortex observe \
  "Rebuild replays datoms through the live neo4j.BackendApplier; \
   hand-rolled Cypher in staging_backends.go was the root of cortex-sv8." \
  --kind=Observation \
  --facets=domain:Architecture,project:cortex,subsystem:rebuild
```

**`--kind`** names the entry type:
`Observation` (neutral fact), `ObservedRace` (a race or failure seen in
the wild), `Decision`, `SessionReflection`, and a handful more in the spec.
When in doubt, `Observation`.

**`--facets`** are `key:value` tuples. `domain:` and `project:` are the two
the retrieval surface actually indexes for cross-project boosts; add more
(module, subsystem, severity, …) if they'll help a future query.

### After the task — REFLECT on patterns

If the session produced multiple related observations, `cortex reflect`
promotes qualifying episodic clusters into semantic frames. This is cheap
to run and keeps the knowledge graph dense. You normally do not need to
invoke it manually — the agent convention is to run it when a trail ends
with 3+ observations that share facets.

---

## Trails — work-session envelopes

A **trail** groups every observation taken during a single work session
into a replayable bundle. At `cortex trail end` time the LLM generates a
narrative summary so a future recall can surface the whole thread, not
just individual datoms.

### The `CORTEX_TRAIL_ID` contract

- `cortex trail begin` prints a new `trail:<ulid>` to stdout **and nothing
  else** — so you can capture it with `$(...)`.
- Your shell exports that value as `CORTEX_TRAIL_ID`.
- Every subsequent `cortex observe` and `cortex trail end` reads the
  environment variable and auto-attaches. You do not pass `--trail`.
- `cortex trail end` reads `CORTEX_TRAIL_ID`, finalizes the trail, and
  exits `NO_ACTIVE_TRAIL` if the variable is unset.

```bash
export CORTEX_TRAIL_ID=$(cortex trail begin \
  --agent=claude-code \
  --name="debug rebuild entry_id mismatch")

cortex observe "rebuild.ApplyDatom wrote id not entry_id" \
  --kind=Observation \
  --facets=domain:Rebuild,project:cortex

cortex observe "fix: delegate to live neo4j.BackendApplier" \
  --kind=Decision \
  --facets=domain:Rebuild,project:cortex

cortex trail end
unset CORTEX_TRAIL_ID
```

### When to begin a trail — and when NOT to

**Begin a trail** when:

- You are starting a debugging session that will produce multiple linked
  observations (root cause + attempts + fix + lessons).
- You are implementing a feature and want the design rationale + the
  specific implementation choices bundled together for future recall.
- You are running a one-off investigation (benchmark, spike, audit) that
  you want future agents to find as a coherent unit.
- The task is likely to span multiple observations over more than a few
  minutes of work.

**Do not begin a trail** when:

- You are writing a single standalone observation (just `cortex observe`
  without a trail).
- The work is pure exploration with nothing worth persisting.
- The task is clearly scoped to one change with no follow-up insight.
- A trail is already active in the environment — check `echo
  $CORTEX_TRAIL_ID` first.

### When to end a trail

Always `cortex trail end` before:

- Claiming a task is complete.
- Switching to an unrelated task.
- The end of a working session.

Leaving a trail open is cheap (just an unset `ended_at`) but the LLM
summary only runs at end time, so un-ended trails never appear in recall
with their narrative context.

---

## Recall result interpretation

A recall result looks like:

```
results=4
  1. entry:01KNWRV6TWQEXW5J82P443WJKC  score=0.429  base=0.661  ppr=0.150  sim=0.619
     Root cause: no jitter on client retry
```

- **score** — final ACT-R activation (the sort key).
- **base** — base-level activation (recency/retrieval count).
- **ppr** — personalized-pagerank score from the seed walk.
- **sim** — cosine similarity against the query vector.

If **sim=0 for every result**, the Weaviate vectors are missing — check
`cortex status` and consider `cortex rebuild --accept-drift` if the
embedding model changed. If **ppr=0 for every result**, the semantic graph
has no edges to walk; this is normal for a cold cache and the ACT-R
rerank still surfaces relevant hits via base+sim alone.

Always read the bodies of the top 2–3 hits before using them as grounds
for a decision — the score ranks by relevance, but the body carries the
actual claim.

---

## Operational footguns

- **Never** pass raw user PII or secrets to `cortex observe`. The write
  pipeline has a secret-scanner that blocks common patterns, but treat it
  as a safety net, not a filter.
- **Do not** delete from `~/.cortex/log.d/`. It is the source of truth;
  backends are rebuildable, the log is not.
- **Never** run `cortex down --purge` without confirming with the user
  first. `--purge` drops the Weaviate + Neo4j volumes; only the datom log
  survives, and the stack has to rebuild from scratch on the next `up`.
- **Before editing** `cmd/cortex/rebuild.go`, `internal/rebuild/`,
  `cmd/cortex/staging_backends.go`, or `cmd/cortex/recall_adapters.go`,
  run the usual impact-analysis routine — those are on the hot path and
  regressions there break every recall.
- **`cortex rebuild` preserves Weaviate vectors** in non-drift mode
  (cortex-sv8). A rebuild that flags `--accept-drift` DOES re-embed and
  swap, which is correct but wipes anything it can't re-derive.

---

## Quick reference

```bash
# Discover what cortex knows about a topic
cortex recall "<topic>" --limit=5

# Record a standalone observation
cortex observe "<claim>" --kind=Observation --facets=domain:X,project:Y

# Bundle a session's worth of observations
export CORTEX_TRAIL_ID=$(cortex trail begin --agent=claude-code --name="<label>")
# ... cortex observe ... cortex observe ...
cortex trail end
unset CORTEX_TRAIL_ID

# Inspect
cortex status
cortex history entry:<ulid>
cortex trail show trail:<ulid>
cortex trail list

# Stack ops
cortex up        # start Weaviate + Neo4j
cortex down      # stop containers, keep volumes
cortex doctor    # run all readiness probes
```

For the exhaustive command set: `cortex help` or `docs/cli-reference.md`.
For the design doc: `docs/spec/cortex-spec.md`.
