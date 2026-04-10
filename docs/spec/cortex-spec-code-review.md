# Cortex Phase 1 — Code Grill Findings (Round 2 + Round 3 addendum)

> Round 3 addendum appended at end of file. Round-2 body preserved verbatim
> below for traceability.

---
# Round 2

**Scope**: `internal/`, `cmd/cortex/`
**Spec**: `docs/spec/cortex-spec.md` (plan-spec format, FR-001..FR-062, SC-001..SC-020)
**Tasks**: `.tasks/cortex-phase1.task.json` (57 tasks) + beads follow-ups
**Mode**: `full-context` (plan-spec + tasks + code)
**Reviewer stance**: read-only; trust nothing reported by the dev agent.
**Build**: `go build ./...` clean.
**Default test suite**: `go test ./...` all packages PASS.
**CLI-exec suite (`-tags=cli`)**: PASS — `internal/e2e/cli_exec_test.go` builds
the real binary and exercises help, retract, subject merge, migrate, and the
notImplemented canaries for recall/reflect/ingest.

Round-1 report header retained below for diff; round-2 deltas are in this
document in the ROUND 2 sections.

---

## Executive Summary (Round 2)

**Verdict: REVISE.** (upgraded from round-1 BLOCK)

Round 1 found a systemic "implemented-but-unwired" failure where every
feature-dev CLI verb was a `notImplemented` stub or returned
`ANALYZE_NOT_WIRED`, and `cortex rebuild` / `cortex bench` silently
succeeded against stub backends. Round 2 confirms that **every round-1
CRITICAL has been materially addressed** and each of the feature CLIs
(`recall`, `reflect`, `ingest`, `retract`, `migrate`, `subject merge`,
`pin/unpin/evict/unevict`, `analyze --find-patterns`) now runs real
adapters against live Neo4j / Ollama / Weaviate clients. `cortex bench`
has been re-wired to exercise real in-process pipelines. The observe
write path now captures `embedding_model_name` / `embedding_model_digest`
datoms and seeds `base_activation` at write time. A CLI-exec test suite
gated by `-tags=cli` now builds the real binary and asserts the
stub-is-gone canaries for round-1's biggest miss.

The remaining gaps are structural and honest: they surface as precise
operational errors (`STAGING_SWAP_NOT_WIRED`, `ErrNoResolver`, etc.)
rather than false success. They are still real blockers on SC-002,
SC-003, SC-004, SC-015, SC-019, and SC-020, but they are *known gaps*
with a clear seam to close, not hidden stubs masquerading as complete
features.

**Why REVISE rather than PASS:**
- One round-1 CRITICAL is only *partially* fixed. Observe still does not
  trigger A-MEM link derivation (FR-011) — `DeriveLinks` has zero call
  sites outside its own test file. This is a flagship Phase 1 behavior.
- Observe still does not wire Neo4j/Weaviate `BackendApplier`s at all
  (MAJ-001). No end-to-end recall can surface any entry because nothing
  ever writes Entry nodes into the graph or Weaviate objects. Recall
  and reflect adapters correctly return empty results against the cold
  graph, but the system as shipped cannot demonstrate any retrieval
  flow end-to-end.
- `cortex rebuild` now writes into a real `:CortexStaging` namespace on
  Neo4j, but `Swap()` is still a sentinel (`STAGING_SWAP_NOT_WIRED`).
  SC-002, SC-019, and SC-020 therefore remain unprovable.
- Startup self-heal (FR-004) is wired into `PersistentPreRunE` but is
  called with **nil appliers** inside `runRootSelfHeal` at
  `cmd/cortex/selfheal.go:103`, so the replay reads watermarks but
  advances nothing. Cosmetically wired; operationally inert.
- Reinforcement datoms (FR-015) returned by `recall.Pipeline.Recall` are
  *discarded* — `cmd/cortex/recall.go:125` has a comment saying the log
  writeback "will be added here once the underlying pipeline produces
  results". As a result every successful recall drops its reinforcement
  update on the floor.

Spec compliance is now **well above** round-1's ~55%: roughly **82%**
of FRs are genuinely end-to-end wired from the CLI through to real
adapters, up from ~55%. The remaining 18% are the gaps listed above —
all of them are honest-NOT_WIRED errors surfaced to the operator rather
than hidden stubs.

---

## Detected Mode

`full-context`: plan-spec + tasks + code. Phases 1, 2, and 3 all activated.

---

## Round 2 — Status of Round-1 Findings

### CRITICAL

| ID | Round 1 | Round 2 | Notes |
|----|---------|---------|-------|
| CRIT-001 `rebuild` writes nothing | BLOCK | **PARTIAL** | `cmd/cortex/staging_backends.go` now writes to a real `:CortexStaging` namespace in Neo4j for Create/Cleanup/ApplyDatom/ApplyEmbedding. `Swap()` still returns `errStagingSwapNotWired`, surfaced to the operator as `STAGING_SWAP_NOT_WIRED`. An honest gap, not a silent stub. SC-002/SC-019/SC-020 still unprovable. |
| CRIT-002 no `embedding_model_digest` at write time | BLOCK | **FIXED** | `internal/write/pipeline.go:427-432` now emits `embedding_model_name` and `embedding_model_digest` attribute datoms on every observed entry; `cmd/cortex/observe.go:161-221` constructs a real `observeEmbedder` backed by the Ollama client. `internal/write/pipeline_test.go:190-208` pins the assertion. |
| CRIT-003 mass `notImplemented` CLI verbs | BLOCK | **FIXED** | Every call-site listed in round 1 now delegates to a real `*Real` constructor. `grep notImplemented cmd/cortex` returns only comments, the definition, and a single dormant helper. Honest wiring confirmed in `recall.go`, `reflect.go`, `ingest.go`, `retract.go`, `migrate.go`, `subject_merge.go`, `lifecycle.go`. |
| CRIT-004 `cortex analyze` returns `ANALYZE_NOT_WIRED` | BLOCK | **FIXED** | `cmd/cortex/analyze.go:96-166` builds a real `analyze.Pipeline` with Neo4j cluster source, Ollama frame proposer, real log writer, and a community-refresher bridge. No unconditional NOT_WIRED. |
| CRIT-005 `cortex bench` stub operations | BLOCK | **FIXED (with caveat)** | `cmd/cortex/bench_harness.go` now builds real `bench.Operation`s: observe against a real `log.Writer` in a temp dir (real append+fsync+flock timing), recall/reflect/analyze against the real `internal/*` pipelines with in-process deterministic adapters. Caveat: bench still doesn't exercise live backends, so SC-006 cannot validate P95 against *actual* Weaviate/Neo4j/Ollama latency; the harness header acknowledges this. |

### MAJOR

| ID | Round 1 | Round 2 | Notes |
|----|---------|---------|-------|
| MAJ-001 observe doesn't apply to Neo4j/Weaviate | BLOCK | **NOT FIXED** | `cmd/cortex/observe.go:163-170` still constructs `write.Pipeline` with `Neo4j` and `Weaviate` nil. Every observe commits only to the log. Recall/reflect/communities have nothing to read in the graph because the write-side applier has never run. This is now the dominant gap behind SC-003/SC-004/SC-008 and is called out by comments in `cmd/cortex/recall_adapters.go:18-23` and `reflect_adapters.go:49-52` ("until the write-side applier lands these return empty"). |
| MAJ-002 self-heal not invoked at startup | BLOCK | **PARTIAL** | `cmd/cortex/main.go:42-47` installs `runRootSelfHeal` as a `PersistentPreRunE` on the root cobra cmd, and `cmd/cortex/selfheal.go` calls `replay.SelfHeal`. BUT: `selfheal.go:103` passes `nil, nil` for the neo4j/weaviate appliers. The self-heal reads watermarks and returns without advancing backend state. Cosmetically wired; operationally a no-op until the write-side appliers land. |
| MAJ-003 `internal/recall|reflect|ingest|migrate|lifecycle|retract` unimported from `cmd/` | BLOCK | **FIXED** | All six packages are now imported by their respective `cmd/cortex/*.go` wiring files. Verified by inspection. |
| MAJ-004 `internal/activation` not called from write path | BLOCK | **FIXED** | `internal/write/pipeline.go:437-443` emits a `base_activation = activation.InitialBaseActivation` attribute datom on every new entry. `internal/write/pipeline_test.go` asserts the datom exists. |
| MAJ-005 no CLI-level e2e test | BLOCK | **FIXED** | `internal/e2e/cli_exec_test.go` exists under `//go:build cli`, builds the real `cortex` binary, and executes version/help/retract/subject merge/migrate plus stub-canaries for recall/reflect/ingest. `go test -tags=cli ./internal/e2e/...` passes. The harness is exactly the shape the round-1 report asked for. |
| MAJ-006 secret-detection coverage through real CLI for ingest/reflect | BLOCK | **PARTIAL** | `cortex ingest` is now wired, so the ingest secret-scan path exercises the real CLI. Reflect runs against LLM-generated frames; the generation-time secret detector path (SC-017) is not asserted by any CLI-exec test. |

### MINOR

| ID | Round 1 | Round 2 | Notes |
|----|---------|---------|-------|
| MIN-001 `observe.go` uses `context.Background()` | MINOR | **NOT FIXED** | `cmd/cortex/observe.go:87` still passes `context.Background()` to `pipeline.Observe`. Signal propagation still broken for observe but the rest of the verbs use `cmd.Context()`. |
| MIN-002 dead `notImplemented` helper retained | MINOR | **PARTIAL** | `commands.go:29-33` still defines `notImplemented` even though nothing calls it. Dead code, easy cleanup. |
| MIN-003 rebuild writer close guarded by `acceptDrift` | MINOR | **FIXED** | `cmd/cortex/rebuild.go:84-92` scopes the writer open+defer to `acceptDrift` explicitly; non-drift path does not open the writer. |

---

## Round 2 — New Findings

### CRIT-006 — FR-011 A-MEM link derivation still completely unwired.

**Severity: CRITICAL.**

`internal/write/links.go` implements `DeriveLinks` with full test coverage
(`links_test.go`, 9 subtests covering cosine floor, confidence floor, type
routing, no-candidates fast path). `grep -rn DeriveLinks` across the
entire repo returns matches only in `internal/write/links.go`, its own
test file, and the round-1 review doc. Nothing calls `DeriveLinks` —
not from `internal/write/pipeline.go`, not from `cmd/cortex/observe.go`,
not from any post-commit hook.

Consequence: every `cortex observe` run writes zero A-MEM link edges.
FR-011 ("write-time A-MEM linking using top-5 nearest neighbors and the
configured confidence thresholds") is satisfied at the library level and
never exercised at the product level. US-2 AS-4 and the traceability
matrix line for FR-011 (`test_observe_amem_link_thresholds`,
`test_observe_malformed_link_output_skips_links`) only cover the
standalone `DeriveLinks` function; neither test drives it through the
observe pipeline.

**Fix direction:** wire a `LinkProposer` + candidate source into
`write.Pipeline` (or into a thin post-commit hook in `cmd/cortex/observe.go`)
and have `write.Pipeline.Observe` call `DeriveLinks` after the log append,
then emit the accepted links as datoms. Needs the Weaviate write-side
applier for "top-5 nearest neighbors" to return anything, so this is
tied to MAJ-001.

### CRIT-007 — Recall reinforcement datoms (FR-015) are discarded.

**Severity: CRITICAL.**

`cmd/cortex/recall.go:123-138` defines `renderRecallResult` and its own
code-comment says:

> // The ReinforcementDatoms slice is returned by the pipeline but the
> // caller is responsible for appending those datoms to the log after
> // rendering — that log write will be added here once the underlying
> // pipeline produces results.

The pipeline returns reinforcement datoms in `res.ReinforcementDatoms`,
but the CLI never opens a log writer and never appends them. FR-015
("default recall writes reinforcement datoms") is therefore not satisfied
end-to-end. Every successful recall loses its activation reinforcement.

**Fix direction:** open a log writer in `buildRecallPipeline` (or run the
append inline in the RunE), group the `ReinforcementDatoms` into one
transaction, and append after rendering.

### MAJ-007 — Self-heal with nil appliers is a cosmetic fix.

**Severity: MAJOR.**

`cmd/cortex/selfheal.go:97-108` calls `replay.SelfHeal(ctx, report.Healthy,
store, nil, nil)`. The header comment admits it: "neo4j and weaviate
appliers are nil pending the cortex-4kq adapter beads that produce
concrete replay.Applier implementations". The round-1 report called this
out as MAJ-002; the round-2 wiring satisfies the *form* (the hook is
installed on `PersistentPreRunE`) without satisfying the *substance*
(state is never advanced). Paired with MAJ-001 (observe doesn't apply to
backends), there is no code path in the shipped binary that can move
state into Neo4j or Weaviate from a user command. FR-004 remains
unmeasurable end-to-end.

### MAJ-008 — `cortex rebuild` Swap is still a sentinel.

**Severity: MAJOR.**

`cmd/cortex/staging_backends.go:202-204` returns `errStagingSwapNotWired`
unconditionally from `Swap()`. `cmd/cortex/rebuild.go:132-135` catches
it and surfaces `STAGING_SWAP_NOT_WIRED` to the operator with a precise
next-step message, which is *honest* — this is no longer a silent stub,
and that's a real improvement over round 1. But SC-002 (byte-identical
Layer 1, structurally identical Layer 2, embedding cosine ≥ 0.98),
SC-019 (two-operator reproducibility), and SC-020 (`down --purge` +
`up` + `rebuild` round-trip) cannot be demonstrated. The fix path is
in the adapter notes and is tracked as a follow-up bead.

### MAJ-009 — Recall EntryLoader returns `Embedding: nil`.

**Severity: MAJOR.**

`cmd/cortex/recall_adapters.go:308-316` sets `Embedding: nil` in every
`recall.EntryState` it builds. The file comment (lines 244-249) explains
that Weaviate vector lookup is deferred. The downstream cosine rerank in
`internal/recall/pipeline.go` will score every candidate as 0, so the
ACT-R blend degenerates and similarity-based ordering cannot work. Tied
to MAJ-001 — once the write-side Weaviate applier lands, this field can
be populated by a per-id GetObject.

### MAJ-010 — No reinforcement CLI wiring + no backend applier means recall has nothing to return.

**Severity: MAJOR.**

Even with CRIT-003/CRIT-004 fully resolved, an operator running
`cortex recall "foo"` against a fresh install will see `results=0` every
time because:

1. `cortex observe` commits to the log but never writes to Neo4j or
   Weaviate (MAJ-001).
2. `cortex rebuild` populates the `:CortexStaging` namespace but cannot
   promote to active (MAJ-008).
3. `runRootSelfHeal` reads watermarks but advances nothing (MAJ-007).

Therefore no code path in the built binary can bring Entry nodes into
the graph that `recall_adapters.go` queries. This is not a new finding
— it's the composition of MAJ-001/007/008 — but it's worth flagging as
a standalone because it bounds what *any* CLI-exec test can prove today:
the surface is honest, but the system is cold. SC-003 and SC-004
therefore remain unprovable via the CLI.

### OBS-001 — Acknowledgement: the CLI surface is now honest.

Round 2 deserves explicit credit: the round-1 failure mode was a suite
of `notImplemented` stubs masquerading as complete work. That failure
mode is gone. Every feature CLI now routes through a real pipeline
constructor, every gap is surfaced as a precise operational error, and
the `cli_exec_test.go` harness prevents regression. The rebuild staging
backend is a particularly good example — instead of pretending success
on a no-op, it writes into a dedicated `:CortexStaging` namespace and
surfaces the swap gap as `STAGING_SWAP_NOT_WIRED` with operator
guidance. That's exactly the behavior the round-1 grill asked for.

---

## Round 2 — Spec Compliance Matrix (delta view)

Status legend carried from round 1. Entries unchanged from round 1 are
omitted for brevity; see round-1 sections above for baseline evidence.

| FR | R1 Status | R2 Status | Evidence |
|----|-----------|-----------|----------|
| FR-004 self-heal | PART (no CLI invocation) | **PART** (installed, nil appliers) | `cmd/cortex/main.go:42-47`, `selfheal.go:103` |
| FR-005 rebuild pinned model | PART (stub backends) | **PART** (real staging, Swap NOT_WIRED) | `cmd/cortex/rebuild.go`, `staging_backends.go` |
| FR-007 retract | PART | **IMPL** (non-cascade) | `cmd/cortex/retract.go`; cascade is `CASCADE_NOT_WIRED` |
| FR-011 A-MEM write-time linking | PART | **MISS** (see CRIT-006) | `DeriveLinks` still has zero call sites outside its tests |
| FR-013 default recall | PART | **PART** (wired, no log writeback for reinforcement, no backend data) | `cmd/cortex/recall.go`; see CRIT-007 |
| FR-014 recall alt modes | PART | **IMPL (wired)** but cold | Covered by `internal/recall/modes.go`; CLI flag not audited this round |
| FR-015 reinforcement datoms | PART | **MISS (CRIT-007)** | Pipeline returns them, CLI discards them |
| FR-017 semantic frames via reflection only | PART | **IMPL** | Wired through `reflect` CLI |
| FR-018 reflection thresholds | PART | **IMPL** | `cmd/cortex/reflect.go` |
| FR-019 per-frame watermark | PART | **IMPL** | `neo4jReflectionWatermarkStore` |
| FR-020 reflect --dry-run --explain | PART | **IMPL** | Flags wired, `renderReflectResult` handles explain |
| FR-022 ingest language strategy | PART | **IMPL** | `cmd/cortex/ingest.go` |
| FR-023 incremental re-ingest | PART | **IMPL** | via `ingest.Pipeline.Resume` |
| FR-024 ingest trail + scoped reflect | PART | **PART** | Trail synthesis wired; post-ingest reflection is "gracefully skipped" per ingest.go header comment |
| FR-025 cross-project opt-in | PART | **IMPL** | Analyze CLI wired with `--find-patterns` gate |
| FR-026..028 cross-project rules | PART | **IMPL (library)** | Analyze pipeline wired, needs backend data to actually fire |
| FR-029 PSI merge | PART | **IMPL** (subject merge CLI) | `cmd/cortex/subject_merge.go` |
| FR-031 seed initial activation | PART | **IMPL** | `internal/write/pipeline.go:437-443` |
| FR-032 pin/unpin/evict/unevict | PART | **IMPL** | `cmd/cortex/lifecycle.go` |
| FR-035 offline operation | PART | **PART** | cli_exec tests don't specifically assert offline; library test still there |
| FR-038 MemPalace migration | PART | **IMPL** | `cmd/cortex/migrate.go` |
| FR-040 bench envelopes | PART (stubs) | **PART** (real in-process ops, not live backends) | CRIT-005 fixed with caveat |
| FR-049 ingest path safety | PART | **IMPL** | Reachable via wired ingest CLI |
| FR-051 digest pinning | MISS | **IMPL** | CRIT-002 fixed |
| FR-052 reflect --contradictions-only | PART | **PART** | Reflect CLI wired but `--contradictions-only` flag not audited this round |
| FR-054 admin command identity audit | PART | **IMPL** | All admin verbs wired and use `defaultActor()` |

Genuine end-to-end coverage: approximately **82%** of FRs (up from ~55%
in round 1). The remaining ~18% is concentrated in the MAJ-001 /
MAJ-007 / MAJ-008 / CRIT-006 / CRIT-007 chain — write-side applier,
recall reinforcement writeback, A-MEM link derivation, and rebuild swap.

### Success Criteria — round 2

| SC | R1 | R2 | Notes |
|----|----|----|-------|
| SC-001 crash survival | lib only | **lib only** | Same — log layer is solid |
| SC-002 rebuild byte-identical | Layer 1 only | **Layer 1 only** | Swap still NOT_WIRED |
| SC-003 cross-project recall | NO | **NO** | Recall CLI wired but backend cold (MAJ-001/010) |
| SC-004 cross-project analyze | NO | **NO** | Analyze CLI wired but backend cold |
| SC-005 doctor | partial | partial | Unchanged, not audited |
| SC-006 bench envelope | NO | **PARTIAL** | Harness runs real ops in process; live-backend envelope still unverifiable |
| SC-007 50-writer contention | YES | YES | Unchanged |
| SC-008 ingest fixture | NO | **PARTIAL** | Ingest CLI wired; real-backend ingest unprovable until applier lands |
| SC-011 no raw secrets | partial | **YES** (via CLI for observe/ingest) | `cortex ingest` wired now |
| SC-015 retract | NO | **YES** (non-cascade) | Cascade path is honest NOT_WIRED |
| SC-016 evict sticky | NO | **YES** | `cortex evict` is honest |
| SC-019 two-operator repro | NO | **NO** | Rebuild swap gap |
| SC-020 down --purge + rebuild | NO | **NO** | Rebuild swap gap |

---

## Round 2 — Task Audit

Round 1 listed 16 tasks as INCOMPLETE/PARTIAL despite being closed in
beads. Round-2 status per task:

| Task | R1 | R2 |
|------|----|----|
| cortex-4kq.33 rebuild-command | INCOMPLETE | **PARTIAL** — real staging writes, Swap is honest sentinel. MAJ-008. |
| cortex-4kq.35 observe-pipeline | PARTIAL | **PARTIAL** — digest capture fixed; A-MEM and backend apply still missing. CRIT-006/MAJ-001. |
| cortex-4kq.36 recall-default-mode | INCOMPLETE | **PARTIAL** — CLI wired, reinforcement writeback missing, cold graph returns empty. CRIT-007/MAJ-010. |
| cortex-4kq.41 amem-link-derivation | INCOMPLETE | **INCOMPLETE** — no change. CRIT-006. |
| cortex-4kq.42 activation decay/reinforcement | PARTIAL | **PARTIAL** — initial-seed datom now written; no decay CLI; reinforcement discarded (CRIT-007). |
| cortex-4kq.43 retract-command | INCOMPLETE | **COMPLETE (non-cascade)** — honest |
| cortex-4kq.44 reflect-command | INCOMPLETE | **COMPLETE** — adapters wired |
| cortex-4kq.48 recall-alt-modes | INCOMPLETE | **COMPLETE** — `internal/recall/modes.go` reachable via CLI |
| cortex-4kq.50 pin/unpin/evict/unevict | INCOMPLETE | **COMPLETE** — `cmd/cortex/lifecycle.go` |
| cortex-4kq.51 ingest-command | INCOMPLETE | **COMPLETE** — `cmd/cortex/ingest.go` |
| cortex-4kq.52 analyze --find-patterns | INCOMPLETE | **COMPLETE** — `cmd/cortex/analyze.go` + adapters |
| cortex-4kq.53 subject-merge | INCOMPLETE | **COMPLETE** — `cmd/cortex/subject_merge.go` |
| cortex-4kq.54 migrate-mempalace | INCOMPLETE | **COMPLETE** — `cmd/cortex/migrate.go` |
| cortex-4kq.55 bench-command | INCOMPLETE | **PARTIAL** — real in-process ops; live-backend envelope not exercised |
| cortex-4kq.56 e2e-cross-project-value-test | PARTIAL | **PARTIAL** — library-drive, still not through binary |
| cortex-4kq.57 e2e-offline-operation-test | PARTIAL | **PARTIAL** — library-drive; cli_exec_test has canaries but no dedicated offline assertion |

Follow-up beads needed:

- **[new]** wire A-MEM link derivation into the observe CLI (CRIT-006).
- **[new]** append reinforcement datoms from `cortex recall` (CRIT-007).
- **[new]** implement the real Neo4j/Weaviate `BackendApplier` and slot
  it into `cmd/cortex/observe.go` + `cmd/cortex/selfheal.go` (MAJ-001,
  MAJ-007, MAJ-009).
- **[new]** complete `realStagingBackends.Swap` via staging Weaviate
  class + atomic swap (MAJ-008).
- Continue refining `MIN-001` (observe uses `cmd.Context()`) and
  `MIN-002` (drop dead `notImplemented` helper).

---

## Round 2 — Test Results

- `go build ./...` — **OK**
- `go test ./...` — **all packages PASS** (cached results used)
- `go test -tags=cli ./internal/e2e/...` — **PASS** (4.068s)
  - `TestCLI_Version` — PASS
  - `TestCLI_RootHelpListsAllVerbs` — PASS
  - `TestCLI_Retract_RequiresEntityID` — PASS
  - `TestCLI_SubjectMerge_RequiresTwoArgs` — PASS
  - `TestCLI_Migrate_RequiresFromMempalace` — PASS
  - `TestCLI_Retract_NotImplementedGone` — PASS (canary green)
  - `TestCLI_SubjectMerge_NotImplementedGone` — PASS
  - `TestCLI_Lifecycle_NotImplementedGone` — PASS
  - `TestCLI_StubCanary_*` for recall/reflect/ingest — PASS (canary green)

---

## Round 2 — Verdict

**REVISE.**

Major upgrade from round 1. Spec compliance is now ~82% genuinely wired
(up from ~55%). All five round-1 CRITICALs are materially addressed,
and the CLI-exec harness (MAJ-005) prevents regression of the
stub-wiring failure mode that motivated round 1.

Two new CRITICALs surface on close inspection:

- **CRIT-006**: FR-011 A-MEM link derivation has a real implementation
  with full tests but zero call sites anywhere in the tree.
- **CRIT-007**: FR-015 recall reinforcement datoms are produced by the
  pipeline but dropped by the CLI — reinforcement never lands on disk.

Plus the structural chain MAJ-001 / MAJ-007 / MAJ-008 / MAJ-009: the
observe write path still does not apply to Neo4j / Weaviate, the
self-heal is called with nil appliers, and `cortex rebuild` cannot
promote staging. This chain keeps SC-002, SC-003, SC-004, SC-019, and
SC-020 unprovable, but the failure is now *honest* — every missing
piece surfaces as a precise operational error, not as false success.

### Action Items

Priority 1 (resolve new round-2 CRITICALs):

1. **CRIT-006** — call `DeriveLinks` from the observe write pipeline
   (or from a thin post-commit hook in `cmd/cortex/observe.go`) and
   emit the accepted links as link datoms. Needs a neighbor source
   which, pragmatically, means the Weaviate applier from MAJ-001 has
   to land in the same bead.
2. **CRIT-007** — append `res.ReinforcementDatoms` to the log from
   `cmd/cortex/recall.go`. Mechanical fix; open a log writer inside
   `buildRecallPipeline`, append after rendering, close on cleanup.

Priority 2 (write-side applier + self-heal follow-through):

3. **MAJ-001** — implement a `BackendApplier` for Neo4j and Weaviate,
   slot it into `buildObservePipeline`, and treat post-commit apply
   errors as recoverable (falls back to the self-heal).
4. **MAJ-007** — once the appliers exist, pass them into
   `runRootSelfHeal` so `replay.SelfHeal` actually advances state.
5. **MAJ-008** — implement `realStagingBackends.Swap` (staging Weaviate
   class + atomic swap) so `cortex rebuild` can complete. Unlocks
   SC-002/SC-019/SC-020.
6. **MAJ-009** — populate `EntryState.Embedding` from Weaviate in
   `neo4jWeaviateEntryLoader.Load` once the applier writes Entry
   objects there.

Priority 3 (finish the round-1 loose ends):

7. **MIN-001** — switch `cmd/cortex/observe.go:87` from
   `context.Background()` to `cmd.Context()`.
8. **MIN-002** — drop the dead `notImplemented` helper in
   `cmd/cortex/commands.go:29-33` now that nothing calls it.
9. **MAJ-006** — add a cli_exec assertion that
   `SECRET_DETECTED_IN_GENERATION` fires through `cortex reflect` on a
   LLM output containing a secret pattern.
10. **FR-052** — audit `cortex reflect --contradictions-only` wiring
    (not verified in this round).

After the CRIT items land, re-run `/grill-code
spec=/Users/nixlim/Sync/PROJECTS/foundry_zero/cortex/docs/spec/cortex-spec.md
round=3`.

---

## Acknowledgements

The round-2 delta is substantial and the team clearly took the round-1
review seriously. Every CRITICAL is at least materially addressed, and
several are closed outright. The design choice to surface unfinished
work as precise `*_NOT_WIRED` operational errors (rather than silent
success paths) is the right one — it keeps the CLI honest and gives
operators an actionable next step. The `internal/e2e/cli_exec_test.go`
harness is the single most important structural change: it converts
the round-1 "closed bead + green unit test = false confidence" failure
mode into an assertion the CI can enforce. The staging backend's
`:CortexStaging` namespace and its `STAGING_SWAP_NOT_WIRED` sentinel
are a particularly clean example of how to represent an honest gap.
Good work on those.

---

# Round 3 Addendum — 2026-04-10

**Scope**: verify the six round-2 action-item commits landed correctly.
**Commits audited**: `cbe5740` (maj-001/007), `f3a7d3b` + `c1e1f13` (crit-006),
`d756db9` (maj-001/007/010 applier layer), `e11e4e4` (maj-009), `6b9e604`
(crit-007), `361ab78` (min-001).
**Mode**: `full-context`.
**Build**: `go build ./...` clean.
**`go test ./...`**: all packages PASS (cached).
**`go test -tags=cli ./internal/e2e/...`**: PASS (cached).

## Executive Summary (Round 3)

**Verdict: BLOCK (re-downgraded).**

The surface of the round-2 action items is all present and the commit
history is honest: every CRIT-006, CRIT-007, MAJ-001, MAJ-007, MAJ-009,
and MIN-001 line item has a corresponding change in the tree and every
touch-point cited in the round-2 action list is populated with non-nil
adapters. CRIT-007 and MIN-001 are genuine end-to-end fixes. The
`internal/weaviate/applier.go`, `internal/neo4j/applier.go`, and
observe/selfheal wiring additions are substantive, well-commented, and
structurally on-spec.

But **the product-level payoff of MAJ-001 + MAJ-009 + CRIT-006 is still
zero**, for three new reasons that the round-2 report did not catch and
the recent fixes introduced or exposed:

1. **CRIT-008 — Schema-key mismatch between the neo4j applier and every
   recall/reflect/analyze Cypher query.** The applier MERGEs nodes as
   `(n:Entry {id: $id})` with property `id` (internal/neo4j/applier.go:145-154).
   Every query that is supposed to read those nodes back — recall's
   loader, seed resolver, PPR stage, reflect adapters, analyze adapters —
   filters on `e.entry_id`. Nothing in the entire repo writes the
   `entry_id` property. Net result: the round-2 claim that "observe now
   applies to Neo4j" is literally true (rows exist), but no recall,
   reflect, or analyze query can ever find them. SC-003, SC-004, SC-008
   remain unprovable for a new reason.

2. **CRIT-009 — Weaviate applier's ApplyWithVector has zero call sites;
   every row lands with a nil vector.** `BackendApplier.Apply` hard-codes
   `vector=nil` on its `Upsert` call (internal/weaviate/applier.go:157-163)
   with a frank comment admitting "the Phase 1 write pipeline does not
   yet hand the float32 vector to Apply". The intended escape hatch
   `ApplyWithVector` (applier.go:174) is defined, tested, and NEVER
   CALLED — `grep -rn ApplyWithVector internal cmd` returns only its own
   file and its test file. Consequence:
     - `weaviateNeighborFinder.Neighbors` will return an empty set for
       any newly-observed entry → CRIT-006's link derivation is live code
       that can never fire against a real graph.
     - `neo4jWeaviateEntryLoader.Load`'s MAJ-009 fix
       (FetchVectorsByCortexIDs) will always return an empty map → cosine
       rerank still degrades to 0, same as before.
   The MAJ-001/MAJ-009 fixes are inert.

3. **MAJ-011 — Write pipeline Stage 7 returns on the first Neo4j apply
   error, contradicting its own contract.** `internal/write/pipeline.go:323-338`
   reads:
   ```go
   if p.Neo4j != nil {
       for i := range group {
           if err := p.Neo4j.Apply(ctx, group[i]); err != nil {
               return result, errs.Operational("NEO4J_APPLY_FAILED", ...)
           }
       }
   }
   if p.Weaviate != nil { ... }
   ```
   The file-local comment on line 321 says "Neo4j and Weaviate are
   independent — a failure in one does not skip the other", but the code
   early-returns the first time Neo4j refuses a datom, so Weaviate is
   skipped AND Stage 8 link derivation is skipped. Any transient Neo4j
   hiccup takes out the entire backend apply phase. This also mid-aborts
   the datom loop, leaving Neo4j in a partial state (some datoms from
   the group applied, some not).

## Round-2 Action Items — Round-3 Status

| R2 Action | R3 Verdict | Evidence |
|-----------|-----------|----------|
| CRIT-006 wire DeriveLinks from observe | **FIXED (inert)** | `internal/write/pipeline.go:339-392` Stage 8 present; `cmd/cortex/observe.go:207-214` populates `Neighbors`, `LinkProposer`, `LinkConfig`, `LinkTopK=5`. BUT Weaviate rows carry no vector (CRIT-009), so `Neighbors()` will return empty and the stage is a no-op in production. |
| CRIT-007 append reinforcement datoms | **FIXED** | `cmd/cortex/recall.go:54-82` opens a `*log.Writer`, calls `appendReinforcementDatoms` after rendering, closes on cleanup. `appendReinforcementDatoms` seals each datom and Appends one group. `cmd/cortex/recall_test.go` asserts the round-trip via `log.NewReader`. Clean. |
| MAJ-001 real Neo4j/Weaviate BackendApplier | **FIXED at library level; INERT at product level** | `internal/neo4j/applier.go` and `internal/weaviate/applier.go` exist, are wired into `cmd/cortex/observe.go:192-196` and `cmd/cortex/selfheal.go:104-106`. Shape is correct. But see CRIT-008 (schema-key mismatch) and CRIT-009 (nil vectors) — the row that lands in Neo4j cannot be read back and the Weaviate row has no vector. |
| MAJ-007 selfheal with real appliers | **FIXED (same inert caveat as MAJ-001)** | `cmd/cortex/selfheal.go:104-106` constructs both appliers and passes them to `replay.SelfHeal`. Self-heal will now actually advance watermarks — but into the same wrong-schema shape as the write path. |
| MAJ-008 realStagingBackends.Swap | **STILL NOT FIXED** | `cmd/cortex/staging_backends.go` — Swap not re-audited this round; round-2 finding stands. |
| MAJ-009 recall EntryLoader embeddings | **FIXED at code level; INERT at product level** | `cmd/cortex/recall_adapters.go:278-288` now calls `FetchVectorsByCortexIDs`; `internal/weaviate/client.go` implements it. But the Weaviate rows never receive vectors in the first place (CRIT-009), so the returned map is always empty. |
| MIN-001 observe `cmd.Context()` | **FIXED** | `cmd/cortex/observe.go:92` passes `cmd.Context()`; verified. |
| MIN-002 dead `notImplemented` helper | **FIXED** | Removed in `8edfb96`; `grep notImplemented cmd/cortex | wc -l` shows only canary-test references. |

## New Round-3 Findings

### CRIT-008 — Neo4j applier writes `n.id`, every consumer reads `n.entry_id`.

**Severity: CRITICAL.**

`internal/neo4j/applier.go:145-154`:
```go
cypher := fmt.Sprintf(
    "MERGE (n:%s {id: $id}) SET n.%s = $value, n.last_tx = $tx",
    label, cypherProperty(d.A),
)
```
Node is merged on property `id` with value `d.E` (e.g., `entry:01H...`).

Every consumer in the tree filters/returns on `entry_id`:
- `cmd/cortex/recall_adapters.go:138-139` (SeedResolver): `WHERE e.entry_id IS NOT NULL RETURN DISTINCT e.entry_id AS id`
- `cmd/cortex/recall_adapters.go:181, 188-189` (PPR): `MATCH (seed) WHERE seed.entry_id IN $seeds` / `RETURN n.entry_id AS id`
- `cmd/cortex/recall_adapters.go:256-272` (Loader): `MATCH (e) WHERE e.entry_id IN $ids ... RETURN e.entry_id AS id`
- `cmd/cortex/reflect_adapters.go:56-58` and `:116`: `WHERE e.entry_id IS NOT NULL`
- `cmd/cortex/analyze_adapters.go:53-55, 109-111, 135`: `WHERE e.entry_id IS NOT NULL`

`grep -rn entry_id internal/neo4j internal/write internal/recall internal/reflect internal/analyze` — no occurrences on the write side. The only place `entry_id` is *written* is `internal/community/list.go`, which is orthogonal.

**Consequence**: every cortex observe lands a row in Neo4j under property
`id`, and every cortex recall/reflect/analyze filters by `entry_id` — a
property that doesn't exist on any node the applier writes. End-to-end
result: `results=0` for every recall, reflect, analyze run, no matter
how many observes preceded them. The round-2 "now writes to Neo4j" fix
does not move the product needle because nothing can read what was
written.

**Fix direction (pick one, apply consistently to both sides):**
- Easiest: write both `id` and `entry_id` in the applier MERGE for
  Entry nodes: `MERGE (n:Entry {id: $id}) SET n.entry_id = $id, ...`.
  Similar for Frame/Subject/etc. (`frame_id`, `subject_id`).
- Better: pick one convention across the codebase and change every
  Cypher query to use it. The plan-spec does not mandate `entry_id`
  specifically, so either direction is defensible; consistency is what
  matters.
- Regardless of direction, add a CLI-exec test
  (`go test -tags=cli ./internal/e2e/...`) that observes an entry and
  then recalls it, asserting `results>=1`. That single test would have
  caught this in round 2.

### CRIT-009 — Weaviate rows are upserted with nil vectors; ApplyWithVector has zero call sites.

**Severity: CRITICAL.**

`internal/weaviate/applier.go:155-167`:
```go
// Vector is empty here: the Phase 1 write pipeline does not yet
// hand the float32 vector to Apply (the embedding lives in the
// pipeline's local var, see internal/write/pipeline.go:309). When
// that wiring lands, the vector will flow through ApplyWithVector
// below; until then the row exists with vectorizer=none and an
// absent vector field.
if err := a.w.Upsert(ctx, class, uuid, nil, snapshot); err != nil {
    return fmt.Errorf("weaviate: apply %s/%s: %w", d.E, d.A, err)
}
```

`ApplyWithVector` is defined at line 174, tested in
`internal/weaviate/applier_test.go`, and **never called anywhere else**
in the repo:
```
$ grep -rn ApplyWithVector internal cmd
internal/weaviate/applier.go:174: func (a *BackendApplier) ApplyWithVector(...)
internal/weaviate/applier.go:169: // ApplyWithVector is the variant the write pipeline calls when it
internal/weaviate/applier_test.go:...
```

The write pipeline constructs the vector in its local `embedding`
variable (`internal/write/pipeline.go:279-285`), hands it to nobody, and
then drops it when `Observe` returns — except for the link derivation
Stage 8 path, which uses it only to look up neighbors, not to persist
it.

**Consequences** (compounding with CRIT-008):
- Every new Entry row in Weaviate has a nil vector field (class is
  `vectorizer=none`, so the row simply has no vector attached).
- `weaviateNeighborFinder.Neighbors(ctx, vector, k)` in
  `cmd/cortex/observe.go:230-250` calls
  `w.client.NearestNeighbors(ctx, weaviate.ClassEntry, vector, k, 0)`.
  NearestNeighbors requires Weaviate to have indexed vectors to
  compare against; with zero indexed vectors across every entry row,
  this call returns empty for the entire lifetime of the deployment.
  **CRIT-006's link derivation Stage 8 is live code that can never fire
  in production.**
- `neo4jWeaviateEntryLoader.Load`'s `FetchVectorsByCortexIDs` lookup
  (the MAJ-009 fix) queries `_additional.vector` on rows that never
  had one. `vectors` map is always empty, `Embedding: vectors[id]` is
  always nil, cosine rerank always contributes 0 — exactly the degraded
  state round 2 claimed was fixed.

**Fix direction:** the write pipeline's Stage 7 needs to call
`ApplyWithVector` for the datom(s) that matter (or at minimum for the
`body` datom on every entry) and pass the captured `embedding` slice.
Rough sketch in `internal/write/pipeline.go`:
```go
if p.Weaviate != nil {
    // Type-assert to the richer shape when available so the write
    // pipeline can feed the vector; fall back to Apply otherwise.
    type vectorApplier interface {
        ApplyWithVector(ctx context.Context, d datom.Datom, v []float32) error
    }
    for i := range group {
        if len(embedding) > 0 && group[i].A == "body" {
            if va, ok := p.Weaviate.(vectorApplier); ok {
                if err := va.ApplyWithVector(ctx, group[i], embedding); err != nil { ... }
                continue
            }
        }
        if err := p.Weaviate.Apply(ctx, group[i]); err != nil { ... }
    }
}
```
Either that, or promote `ApplyWithVector` into the `BackendApplier`
interface with a default implementation that falls through to `Apply`
with a nil vector.

### MAJ-011 — Stage 7 early-returns on first Neo4j error, contradicts its own comment and skips Weaviate + Stage 8.

**Severity: MAJOR.**

`internal/write/pipeline.go:317-338`:
```go
// Neo4j and Weaviate are independent — a failure in one
// does not skip the other. ---
if p.Neo4j != nil {
    for i := range group {
        if err := p.Neo4j.Apply(ctx, group[i]); err != nil {
            return result, errs.Operational("NEO4J_APPLY_FAILED", ...)
        }
    }
}
if p.Weaviate != nil {
    for i := range group {
        if err := p.Weaviate.Apply(ctx, group[i]); err != nil {
            return result, errs.Operational("WEAVIATE_APPLY_FAILED", ...)
        }
    }
}
```

Four problems:
1. A Neo4j error on the first datom of the group returns immediately and
   skips the remaining datoms. The graph is left in a partial state for
   the entity (e.g., `id` set but `body` missing). Idempotent replay
   will catch up, but the CLI reports `NEO4J_APPLY_FAILED` without
   mentioning the partial-apply.
2. Neo4j failure skips the entire Weaviate apply loop, contradicting
   the inline comment claiming independence.
3. Neo4j failure skips Stage 8 link derivation. A transient Cypher
   error on a single stale edge takes out A-MEM linking for the whole
   invocation.
4. The same structural problem applies to Weaviate → Stage 8: a
   Weaviate apply error will also early-return, even though Stage 8's
   documented contract is "best-effort, never affects the source entry".

**Fix direction:** collect per-datom errors into a per-backend
`[]error`, keep going through both loops, surface the first one (or a
wrapped multi-error) at the end, and ONLY short-circuit Stage 8 if the
Neo4j/Weaviate combined state is known to be inconsistent (which Phase
1 can't really prove — so safer to just always run Stage 8 post-apply).

### OBS-002 — The tests are still green because nothing exercises the observe→recall round trip end-to-end.

`go test ./...` and `go test -tags=cli ./internal/e2e/...` both pass.
Neither CRIT-008 nor CRIT-009 nor MAJ-011 is caught by any test in the
tree. The existing unit tests for the appliers use fake `graphWriter` /
`objectUpserter` interfaces that don't execute real Cypher and don't
care which property key the MERGE clause uses. The existing cli-exec
tests assert the subcommand surface, not that a recall can see an
observe.

**Recommendation (not a blocker but a repeat of MAJ-005's spirit):** add
one end-to-end test, gated behind the `cli` build tag and either the
`neo4j` integration tag or a `dockertest` harness, that:
1. Runs `cortex observe --kind Observation --facets domain:x,project:y "hello world"`.
2. Runs `cortex recall "hello"` and asserts `results>=1`.
That single test makes CRIT-008 and CRIT-009 impossible to ship.

## Round-3 Verdict

**BLOCK.**

This is a re-downgrade from round-2's REVISE. The honest summary:

- CRIT-007 (recall reinforcement) and MIN-001 (observe context) are
  genuinely fixed and close cleanly.
- CRIT-006, MAJ-001, MAJ-007, and MAJ-009 have all landed at the code
  level — the adapters, the wiring, the comments, and the
  buildObservePipeline/selfheal hookups are present and shaped
  correctly — but the product outcome is still broken for reasons that
  round 2 did not catch:
    - Nothing the neo4j applier writes can be read by any consumer,
      because the applier writes `id` and the consumers all read
      `entry_id` (CRIT-008).
    - Nothing Weaviate stores has a vector, because the write pipeline
      never calls `ApplyWithVector` and the generic `Apply` hard-codes
      `vector=nil` (CRIT-009).
    - Stage 7 early-returns on the first Neo4j error, contradicting
      its own comment (MAJ-011).
- MAJ-008 (`realStagingBackends.Swap`) is still not resolved from
  round 2; re-audit not required this round.

Net effect: SC-002, SC-003, SC-004, SC-008, SC-019, SC-020 all remain
unprovable, but now for three new reasons that are easier to fix than
the round-1 and round-2 findings were. None of CRIT-008/009 or MAJ-011
is a deep architectural problem; each is a ~10-line diff away from
being closed.

### Round-3 Action Items

**Priority 1 (new CRITICALs):**

1. **CRIT-008** — unify the Neo4j node-id property across the applier
   and every consumer. Pick one of: (a) applier writes both `id` and
   `entry_id`/`frame_id`/etc, or (b) every Cypher consumer switches to
   `n.id`. Apply the same fix to `body`, `trail_id`, `community_id`,
   and any other mismatched property the applier uses with
   `cypherProperty(d.A)`.
2. **CRIT-009** — make the write pipeline actually pass the embedding
   vector to Weaviate. The minimum fix is a type-assertion for
   `ApplyWithVector` inside Stage 7, guarded by `group[i].A == "body"`
   so each entity gets exactly one vector upsert. Promote
   `ApplyWithVector` to a first-class method on the `write.BackendApplier`
   interface if the assertion feels fragile.
3. **MAJ-011** — rewrite Stage 7 so Neo4j and Weaviate apply loops are
   independent, accumulate errors instead of early-returning, and
   always run Stage 8 regardless of per-backend error state.

**Priority 2 (test harness):**

4. **Follow-up bead** — add an integration-tagged test
   (`//go:build cli && integration`) that runs observe + recall
   end-to-end against a real (or dockertested) Neo4j + Weaviate +
   Ollama and asserts the round trip. This is the only defense against
   round-4 finding another "fixed in code but inert in product" bug.

**Priority 3 (round-2 loose ends that are still open):**

5. **MAJ-008** — `realStagingBackends.Swap` still sentinel. No change
   this round.
6. **MAJ-006** — cli_exec `SECRET_DETECTED_IN_GENERATION` coverage for
   reflect. Still open.
7. **FR-052** — `cortex reflect --contradictions-only` flag audit.
   Still deferred.

After CRIT-008, CRIT-009, and MAJ-011 land, re-run
`/grill-code spec=/Users/nixlim/Sync/PROJECTS/foundry_zero/cortex/docs/spec/cortex-spec.md round=4`.

## Acknowledgements (Round 3)

The round-2 → round-3 delta shows real work: the two applier packages
(`internal/neo4j/applier.go`, `internal/weaviate/applier.go`) are new
code, well-commented, and land the abstraction on the correct side of
the `replay.Applier` / `write.BackendApplier` seam. `cmd/cortex/recall.go`
is a particularly clean CRIT-007 fix — the log writer is owned at the
CLI layer, the pipeline stays pure, and the on-disk round trip is
pinned by a unit test. The same "honest operational errors instead of
silent success" philosophy that carried round 2 carries through here
too: every remaining gap surfaces as a precise envelope (`NEO4J_APPLY_FAILED`,
`WEAVIATE_APPLY_FAILED`, `REINFORCEMENT_APPEND_FAILED`).

The round-3 failure mode is different from round 2's: the code is
written, the adapters are real, and the wiring is present. What is
missing is cross-package *consistency* — one side of a seam writes
`id`, the other reads `entry_id`; one side constructs an `embedding`
local, the other never receives it. The fix is mechanical; the lesson
is that unit-test-level coverage of each side in isolation is not
enough to catch seam-level drift, which is exactly what a
`cli+integration` tagged end-to-end test would prevent.
