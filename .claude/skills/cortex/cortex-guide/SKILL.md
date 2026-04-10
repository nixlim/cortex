---
name: cortex-guide
description: Cortex is YOUR persistent memory across sessions in this repo. Use to recall prior observations before any non-trivial task and to record decisions, root causes, and discoveries during work. MUST consult on every task that touches code, architecture, design decisions, bug investigations, performance work, or anything the user asks "how do we", "why did we", "what did we decide", "do we already". MUST write observations at every natural checkpoint (root cause found, decision made, bug fixed, benchmark run, surprising behavior discovered).
---

# Cortex is YOUR memory

Cortex is the local knowledge substrate this repo ships. It is **not a
user-facing tool** — it is the persistent memory you, as an agent, use to
carry knowledge forward across sessions. Each new chat you start with the
user has no continuity to prior chats unless you put that continuity into
cortex. Your past self wrote observations so you could find them. Write
observations now so your future self has them.

Everything below is a standing instruction that applies for the entire
session, not a one-time checklist.

---

## The two things you MUST do

### 1. RECALL first, every time

Before you start any non-trivial work, run `cortex recall` with the most
specific phrase you can extract from the user's request. This is
non-optional. You do it even if you are "sure" you know the answer,
because past-you frequently discovered non-obvious constraints that
override your first instinct.

```bash
./cortex recall "<specific phrase from the request>" --limit=5
```

Triggers — any one of these means you MUST recall before responding:

- The user asks about a subsystem by name (`recall`, `rebuild`, `write pipeline`, …).
- The user asks "how does X work?", "why does X do Y?", "what decides Z?".
- The user reports a bug or an unexpected behavior.
- You are about to edit a file you haven't seen this session.
- The user asks about a previous session ("last time we worked on…", "the bug you fixed…").
- You are about to propose a refactor or architectural change.
- The user asks for performance work, benchmarks, or optimization.

If recall returns nothing, say so in one line and proceed. Do **not**
silently skip the recall step — the user will see the hook-injected
recall context anyway, and absent recall output signals a missed check.

Read the top 2-3 bodies in full. The score ranks by relevance, but the
body carries the actual claim and the claim is what you act on.

### 2. OBSERVE every valuable finding

When work produces a fact that a future agent (you, in a new chat) would
want to find, you write that fact to cortex. Do **not** wait for the user
to ask. Do **not** assume the git log will preserve it — the git log
preserves the diff, not the reasoning.

```bash
./cortex observe \
  "<one-sentence claim stating the fact>" \
  --kind=<Observation|ObservedRace|SessionReflection> \
  --facets=domain:<X>,project:cortex,subsystem:<Y>
```

MUST-observe triggers — any one of these means you write an observation
before moving on:

- **Root cause found.** You identified why a bug happened. Observe the
  root cause as `--kind=Observation`, not the symptom.
- **Decision made.** You chose approach A over approach B, with reasons.
  Observe as `--kind=Observation` with `kind-of-record:decision` in
  the facets. `Decision` is NOT a valid `--kind` value — the `design_decision`
  frame exists but is reflection-only and is populated automatically
  when `cortex reflect` finds enough exemplars to promote a pattern.
  Put the trade-off in the body.
- **Bug fixed.** The fix isn't in the commit message — the bug's invariant
  is. Observe the invariant.
- **Benchmark run.** You measured performance (p50/p95/p99, envelope, n).
  Observe the numbers and the conditions.
- **Surprising behavior discovered.** A library, a tool, or the
  operating system did something you didn't expect. Observe it.
- **Architecture constraint learned.** You found that X can only be done
  after Y because of Z. Observe the constraint.
- **Failing path identified.** You reproduced a race, a deadlock, an
  edge case. Observe it as `--kind=ObservedRace`.
- **Docs or spec written.** Writing a README section, spec, ADR, or
  design doc forces articulation of intent and frequently surfaces
  gaps the code didn't expose. Observe the intent you articulated (as
  `--kind=Decision` if it codifies a decision, `--kind=Observation` if
  it captures an invariant) AND any missed items or inconsistencies
  the writing exposed (`--kind=Observation`, one per item). Docs work
  is often more observation-rich than code work — treat it that way.
- **Config changed.** Edits to `docker-compose.yaml`, `config.yaml`,
  Dockerfiles, Makefiles, CI definitions, or `.claude/settings.json`
  usually encode an invariant ("this envelope", "this timeout",
  "this hook order"). Observe the invariant, not the diff.

A good observation is one sentence that states a claim a future agent can
act on without reading the source.

**Good:**
> `cortex rebuild replays datoms through the live neo4j.BackendApplier; a
> parallel Cypher translator in staging_backends.go drifted from the live
> shape and caused cortex-sv8 (Entry.id vs Entry.entry_id mismatch).`

**Bad:**
> `Fixed rebuild bug.` (no claim)
> `I changed staging_backends.go to use BackendApplier.` (describes the
> diff, not the invariant)

Facets matter for future retrieval. Always set `project:cortex` and at
least one `domain:<area>` facet. Add `subsystem:`, `kind:`, `severity:`,
or custom keys whenever they'll help a query narrow in.

---

## Trails — bundle multi-observation work

A **trail** groups a session's worth of observations under one
LLM-generated summary. You open a trail at the start of any task that
will produce more than one observation, and you close it at the end so
the summary gets written.

```bash
export CORTEX_TRAIL_ID=$(./cortex trail begin \
  --agent=claude-code \
  --name="<what you are about to do>")

# ... do the work, writing observations along the way.
# Every `./cortex observe` auto-attaches to CORTEX_TRAIL_ID.

./cortex trail end
unset CORTEX_TRAIL_ID
```

### When to begin a trail (mandatory)

- Debugging a bug that takes more than ~5 minutes or produces more than
  one observation.
- Implementing a feature (design rationale + implementation choices
  belong together).
- Running a benchmark, spike, or audit.
- Any task the user explicitly frames as a "session" or "investigation".

### When NOT to begin a trail

- You are writing a single standalone observation.
- A trail is already active (`echo $CORTEX_TRAIL_ID`). Nested trails are
  not supported.
- You cannot guarantee you will reach the end of the task (e.g. a
  user question that may cancel mid-work).

### Always end trails you open

A trail that is never ended still has its datoms, but `cortex trail end`
is the only thing that runs the LLM summarizer. Un-ended trails don't
surface with narrative context in future recalls. **Always** end a trail
before claiming a task is complete, before switching to an unrelated
task, and before the session ends.

---

## Ingest — keep the corpus fresh

`./cortex ingest --project=cortex <path>` walks the repo, summarizes
each module with Ollama, and writes one observation per module. This is
the bulk-capture path that lets recall surface code-level context.

You do not need to run ingest on every session. The session-start hook
checks whether the last ingest matches the current `git rev-parse HEAD`;
if it reports staleness, either:

- **Run ingest** when the user is about to do architecture work, a
  large refactor, or anything that benefits from fresh module summaries:
  ```bash
  ./cortex ingest --project=cortex --commit=$(git rev-parse HEAD) .
  ```
- **Skip ingest** when the user is doing a small bug fix or docs change
  — stale module summaries don't block that work.

Ingest is idempotent: re-running it only re-summarizes modules whose
files have changed. A full fresh ingest of this repo takes a few
minutes; a re-ingest after small edits is near-instant.

---

## Automatic observations

The repo has a post-commit hook that auto-observes every `git commit`
you make during this session. The hook extracts the commit SHA, subject,
and body and writes one `Observation` entry with
`domain:Repo,project:cortex,kind-of-record:commit,commit:<sha>` facets.
You do **not** need to observe the fact that you committed — the hook
handles that.

You **do** still need to observe:

- The **root cause** of whatever the commit fixed (the commit message
  describes the fix; the observation describes the invariant the fix
  restores).
- Any **decisions** you made during the work that didn't land in the
  commit message.
- Any **surprising behavior** you discovered while working.
- Any **benchmark numbers** the commit produced.

The post-commit hook is a floor, not a ceiling.

---

## Recall result interpretation

```
results=4
  1. entry:01KNWRV6TWQEXW5J82P443WJKC  score=0.429  base=0.661  ppr=0.150  sim=0.619
     Root cause: no jitter on client retry
```

- **score** — final ACT-R activation (the sort key).
- **base** — base-level activation (recency × retrieval count).
- **ppr** — personalized pagerank score from the seed walk.
- **sim** — cosine similarity against the query vector.

Signals to notice:

- **sim=0 across the board** — Weaviate has no vectors. Check
  `./cortex status`; if Weaviate is up, consider `./cortex rebuild
  --accept-drift` (but confirm with the user first — it re-embeds
  everything).
- **ppr=0 across the board** — the semantic graph is cold or the
  concept extraction missed. Results may still be useful via
  base+similarity alone.
- **Same 3 entries surface for every query** — their base activation is
  dominating. They may be genuinely relevant, or the corpus is too thin.
- **TrailContext populated** — the entry belongs to a trail. The trail
  summary may contain the "why" behind the "what". Run
  `./cortex trail show <trail-id>` to read the full bundle.

---

## Operational rules — never violate

- **Never** delete from `~/.cortex/log.d/`. The log is the source of
  truth. Backends are rebuildable; the log is not.
- **Never** run `./cortex down --purge` without explicit user
  confirmation. `--purge` drops Weaviate + Neo4j volumes.
- **Never** write PII, secrets, or credentials to `./cortex observe`.
  The write pipeline has a secret-scanner, but treat it as a safety net.
- **Never** observe transient task state ("I am about to edit X"). Use
  a plan or in-conversation notes for that.
- **Never** observe the user's prompt verbatim. Observe the *claim* you
  derived from the work, not the request that prompted it.
- **Before editing** `cmd/cortex/rebuild.go`, `cmd/cortex/staging_backends.go`,
  `cmd/cortex/recall_adapters.go`, or `internal/rebuild/`: recall the
  recent work on those files first — they are on the hot path and
  regressions there break every recall.

---

## Quick reference

```bash
# MUST do at the start of any non-trivial task
./cortex recall "<specific phrase>" --limit=5

# MUST do when a fact worth persisting appears
./cortex observe "<claim>" --kind=<kind> --facets=domain:<X>,project:cortex

# Multi-observation tasks — begin and end a trail
export CORTEX_TRAIL_ID=$(./cortex trail begin --agent=claude-code --name="<task>")
# ... work ...
./cortex trail end
unset CORTEX_TRAIL_ID

# Inspect
./cortex status
./cortex history entry:<ulid>
./cortex trail show trail:<ulid>
./cortex trail list
./cortex ingest status --project=cortex

# Keep the corpus fresh (run when stale + architecture work pending)
./cortex ingest --project=cortex --commit=$(git rev-parse HEAD) .
```

For exhaustive command reference: `./cortex help` or `docs/cli-reference.md`.
For the design doc: `docs/spec/cortex-spec.md`.
