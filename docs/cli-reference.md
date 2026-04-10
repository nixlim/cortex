# Cortex CLI Reference

Complete reference for every `cortex` subcommand in Phase 1. Generated
from the `cortex 0.1.0` binary and verified against a live stack on
2026-04-10.

**Global conventions**

- Every command that reads or writes persistent state honours
  `~/.cortex/config.yaml` (auto-created by `cortex up` on first run).
- Standard exit codes: `0` success, `1` operational failure, `2`
  validation failure.
- Most commands accept `--json` for machine-readable output.
- The only environment variable Cortex reads is **`CORTEX_TRAIL_ID`** —
  set by the shell after `cortex trail begin` so subsequent `cortex
  observe` and `cortex trail end` calls can attach to it.
- Subcommands that touch the backends trigger the startup self-heal
  protocol first. `version`, `up`, `down`, `status`, and `doctor` skip
  self-heal so they can run against a half-broken stack.

---

## Lifecycle

### `cortex up`

Start the managed Docker stack (Weaviate, Neo4j+GDS), wait for each
service's readiness endpoint, probe the host Ollama, and return success
only once the 90-second startup budget has been satisfied.

```text
cortex up
```

On first run, also: generates a random Neo4j password (persisted to
`~/.cortex/config.yaml` mode `0600`), builds the `cortex/neo4j-gds:0.1.0`
image, and threads `NEO4J_PASSWORD` into the compose subprocess.

Failure codes: `DOCKER_UNREACHABLE`, `COMPOSE_FAILED`,
`WEAVIATE_NOT_READY`, `NEO4J_NOT_READY`, `GDS_NOT_AVAILABLE`,
`OLLAMA_NOT_REACHABLE`, `OLLAMA_MODEL_MISSING`, `STARTUP_BUDGET_EXCEEDED`.

### `cortex down`

Stop managed containers while preserving named volumes. `~/.cortex/log.d/`
is never touched.

```text
cortex down [--purge]
```

| Flag       | Description                                                 |
|------------|-------------------------------------------------------------|
| `--purge`  | Also remove the Weaviate and Neo4j volumes after an interactive confirmation prompt. |

### `cortex status`

Report each managed dependency as running/stopped/degraded with version,
log watermark, entry count, and disk usage. Shallow and fast (< 2 s).
Deep checks belong to `cortex doctor`.

```text
cortex status [--json]
```

### `cortex doctor`

Run diagnostic checks across Cortex dependencies.

```text
cortex doctor [--quick | --full] [--json]
```

| Flag       | Description                                       |
|------------|---------------------------------------------------|
| `--quick`  | Bounded-time checks only (< 5 s total).           |
| `--full`   | Adapter probes, segment scan, watermark drift, quarantine count, permission audit, disk space, host prerequisites. Uses `doctor.parallelism` workers. |

Checks emit one line each with `pass`, `warn`, or `fail` plus a
remediation hint.

---

## Writes

### `cortex observe`

Write a validated episodic entry through the standard pipeline
(validate → secret scan → PSI resolve → embed → append → apply).

```text
cortex observe <body> --kind=<Observation|SessionReflection|ObservedRace>
  --facets=key:value,key:value [--subject=<psi>] [--trail=<trail-id>] [--json]
```

| Flag          | Description                                                      |
|---------------|------------------------------------------------------------------|
| `--kind`      | Frame type. Must be `Observation`, `SessionReflection`, or `ObservedRace`. Reflection-only kinds are rejected with `REFLECTION_ONLY_KIND`. |
| `--facets`    | Comma-separated `key:value` list. MUST include `domain` and `project`. |
| `--subject`   | Optional PSI subject to attach (canonical or alias). Namespace prefixes enforced: `lib/`, `cve/`, `bugclass/`, `concept/`, `principle/`, `adr/`, `service/`. |
| `--trail`     | Optional trail id. Falls back to the `CORTEX_TRAIL_ID` env var.  |
| `--json`      | Emit machine-readable JSON error envelope on failure.            |

**Success**: prints the new `entry:<ulid>` on stdout and exits 0.
**Validation failure**: exits 2 with an error envelope.
**Backend apply failure**: exits 1 with `BACKEND_APPLY_PARTIAL`; the log
commit is authoritative and self-heal will retry on the next command.

Example:

```bash
cortex observe "Retries must use exponential backoff with full jitter" \
  --kind=Observation \
  --facets=domain:Reliability,project:pay-gw
```

### `cortex trail`

Manage episodic trails. Subcommands: `begin`, `end`, `show`, `list`.

#### `cortex trail begin`

Mint a new trail and print its id.

```text
cortex trail begin --agent=<name> --name=<label> [--json]
```

Capture the printed trail id into `CORTEX_TRAIL_ID` so subsequent
`observe` and `trail end` calls can attach to it:

```bash
export CORTEX_TRAIL_ID=$(cortex trail begin --agent=my-agent --name="debug retry storm")
```

#### `cortex trail end`

Read `CORTEX_TRAIL_ID` from the environment, materialize the trail's
entries, ask the host generation model for a narrative summary, and
append `ended_at` + `summary` datoms.

```text
cortex trail end [--json]
```

Exits 2 with `NO_ACTIVE_TRAIL` when `CORTEX_TRAIL_ID` is unset.

#### `cortex trail show`

```text
cortex trail show <trail-id> [--json]
```

Prints id, name, agent, timestamps, summary, and the ordered list of
member entries.

#### `cortex trail list`

```text
cortex trail list [--limit=N] [--offset=N] [--json]
```

Lists trails in reverse chronological order with stable pagination.

### `cortex retract`

Write an append-only retraction tombstone against an entity. Nothing is
ever deleted — the original assertions remain visible to `cortex history`
and `cortex as-of`, while default recall hides retracted entities.

```text
cortex retract <entity-id> [--cascade] [--reason=<text>] [--json]
```

| Flag         | Description                                                          |
|--------------|----------------------------------------------------------------------|
| `--cascade`  | Also retract `DERIVED_FROM` descendants (requires graph resolver). |
| `--reason`   | Operator-supplied retraction reason; recorded in audit datom.       |

---

## Reads

### `cortex recall`

Run the default-mode retrieval pipeline: concept extraction → seed
resolution → Personalized PageRank → entry load → ACT-R activation
rerank → trail/community context attachment. Every surfaced entry is
reinforced with an activation-update datom.

```text
cortex recall <query> [--limit=N] [--json]
```

| Flag        | Description                                    |
|-------------|------------------------------------------------|
| `--limit`   | Override `retrieval.default_limit` (default 10). |
| `--json`    | Emit machine-readable JSON output.              |

Note: the alternate recall modes described in the spec
(`similar`, `traverse`, `path`, `community`, `surprise`) are not yet
exposed on the CLI in Phase 1. Only default mode is wired.

### `cortex history`

Walk the segmented datom log and print every datom whose entity field
equals the supplied id, in tx-ascending order. The command never
collapses by attribute — activation reinforcements, retractions, and
every other attribute are preserved verbatim.

```text
cortex history <entity-id> [--json]
```

### `cortex as-of`

Print the entries visible at the given transaction id. Visibility is
restricted to datoms whose tx ≤ the supplied tx.

```text
cortex as-of <tx-id> [--json]
```

Exits 1 with `NOT_FOUND` if the tx id is unknown.

### `cortex communities`

List every persisted Community at the requested hierarchy level with
member count and (when present) the LLM-generated summary. Communities
below the minimum size floor are suppressed.

```text
cortex communities [--level=N] [--json]
```

| Flag        | Description                                      |
|-------------|--------------------------------------------------|
| `--level`   | Hierarchy level to list (0 = leaves).             |

### `cortex community show`

Fetch one Community node by its `L<level>:C<id>` token (as printed by
`cortex communities`) and render its member count, summary, and
entry-prefixed member ids.

```text
cortex community show <community-id> [--json]
```

---

## Reflection & analysis

### `cortex reflect`

Consolidate episodic clusters into typed frames. Reads the per-frame
reflection watermark, asks the cluster source for candidates committed
after it, applies the four threshold rules (size, distinct timestamps,
cosine floor, MDL ratio), asks the LLM to propose a frame, and on
acceptance appends frame datoms and advances the watermark.

```text
cortex reflect [--dry-run] [--explain] [--json]
```

| Flag          | Description                                              |
|---------------|----------------------------------------------------------|
| `--dry-run`   | Evaluate candidates without writing frames.              |
| `--explain`   | Print rejection reasons per candidate.                   |

### `cortex analyze`

Cross-project pattern analysis. Accepts clusters spanning at least 2
distinct projects with no more than 70% of exemplars from any single
project, applies the relaxed MDL ratio of 1.15, marks accepted frames
`cross_project=true`, boosts importance by +0.20, and performs a
community refresh.

```text
cortex analyze [--find-patterns] [--dry-run] [--explain] [--include-migrated] [--json]
```

| Flag                   | Description                                                     |
|------------------------|-----------------------------------------------------------------|
| `--find-patterns`      | Enable cross-project pattern finding.                            |
| `--dry-run`            | Evaluate candidates without writing frames.                     |
| `--explain`            | Print rejection reasons per candidate.                          |
| `--include-migrated`   | Include migrated entries in cluster selection (default excluded). |

---

## Ingestion

### `cortex ingest`

Walk a project root, group files by language strategy, summarize each
module with Ollama, write one episodic entry per module, and synthesize
an ingest trail. Re-runs are idempotent.

```text
cortex ingest <path> --project=<name> [--commit=<sha>] [--dry-run] [--resume] [--json]
```

| Flag          | Description                                                    |
|---------------|----------------------------------------------------------------|
| `--project`   | Required project name for scoping.                            |
| `--commit`    | Optional commit SHA to record in ingest state.                 |
| `--dry-run`   | Walk + summarize without writing entries.                     |
| `--resume`    | Resume an earlier interrupted run (same as re-running).        |

#### `cortex ingest resume`

```text
cortex ingest resume <path> --project=<name> [--json]
```

Process only missing modules for a project.

#### `cortex ingest status`

```text
cortex ingest status --project=<name> [--json]
```

Report last-ingested commit and counts.

---

## Activation control

| Command                      | Effect                                                                                       |
|------------------------------|----------------------------------------------------------------------------------------------|
| `cortex pin <entity-id>`     | Sticky-pin an entry; activation never decays below the pin-time value.                       |
| `cortex unpin <entity-id>`   | Remove a pin.                                                                                |
| `cortex evict <entity-id>`   | Force `base_activation=0` and write a sticky `evicted_at` marker so the entry disappears from default recall. |
| `cortex unevict <entity-id>` | Retract the eviction marker.                                                                  |

All four support `--json`.

---

## Subject registry

### `cortex subject merge`

Accretively merge two subjects by writing append-only alias datoms.
Nothing is mutated or deleted; recall follows the alias edge to the
canonical form. Facet contradictions are preserved on a contradiction
edge.

```text
cortex subject merge <canonical-psi> <alias-psi> [--json]
```

---

## Log maintenance

### `cortex export`

Serialize the merged tx-sorted datom stream from `~/.cortex/log.d` to
stdout (or to `--out=<path>`, created with mode `0600`). The format is
canonical JSONL — byte-identical to a fresh segment write.

```text
cortex export [--out=<path>]
```

### `cortex merge`

Validate every datom's checksum in each external segment file and
rename it into the local log directory. The merge-sort reader handles
deduplication. A checksum failure leaves the external file untouched
and exits 1.

```text
cortex merge <segment-path>...
```

### `cortex rebuild`

Replay every committed datom into a clean Weaviate and Neo4j using the
`embedding_model_digest` recorded at write time. By default, fails
loudly if the current model digest differs from any stored entry's
pinned digest.

```text
cortex rebuild [--accept-drift] [--json]
```

| Flag              | Description                                                          |
|-------------------|----------------------------------------------------------------------|
| `--accept-drift`  | Re-embed under the current model and write `model_rebind` audit datoms. |

### `cortex migrate`

Read a MemPalace JSONL export and write one Cortex entry per record
through the standard observe pipeline. Drawers become `Observation`
entries; diaries become `SessionReflection` entries anchored on a
synthesized trail. Every migrated entry carries `migrated=true` and
`source_system=mempalace` facets.

```text
cortex migrate --from-mempalace=<path> [--default-domain=<name>] [--default-project=<name>] [--json]
```

---

## Benchmarks

### `cortex bench`

Execute the scripted benchmark sequence under the chosen profile and
corpus size, writing the JSON report to `~/.cortex/bench/latest.json`.
Exit 0 means every operation passed its envelope; exit 1 means at
least one failed.

```text
cortex bench [--profile=P1-dev|P1-ci] [--corpus=small|medium] [--live] [--json]
```

| Flag           | Description                                                                                             |
|----------------|---------------------------------------------------------------------------------------------------------|
| `--profile`    | Envelope profile: `P1-dev` (default) or `P1-ci`.                                                         |
| `--corpus`     | Fixture corpus size: `small` (default) or `medium`.                                                      |
| `--live`       | Route `observe` and `recall` through the live Weaviate / Neo4j / Ollama stack (requires `cortex up`). The gate probes each backend and fails fast with `BENCH_BACKEND_NOT_READY` on an unprepared stack. |

Default (in-process mode) exercises real validation, scoring, and
threshold logic against deterministic stubs so a regression in the
core Go packages still shows up as a latency delta.

---

## `cortex version`

Print the Cortex release version (currently `0.1.0`).

```text
cortex version
```

---

## Exit codes

| Code | Meaning                                                  |
|------|----------------------------------------------------------|
| `0`  | Success.                                                  |
| `1`  | Operational failure (backend unreachable, IO error, …).  |
| `2`  | Validation failure (bad flag, missing facet, unknown kind, …). |

Error envelopes (with `--json`) include a stable `code` field that maps
1:1 to the underlying error — e.g. `EMPTY_QUERY`, `MISSING_KIND`,
`EMBEDDING_DIM_MISMATCH`, `BACKEND_APPLY_PARTIAL`, `NO_ACTIVE_TRAIL`,
`DOCKER_UNREACHABLE`, `STARTUP_BUDGET_EXCEEDED`. The full list is
enumerated under the `Code*` constants in `internal/infra/up.go` and
as string literals in each subcommand's `errs.Validation` /
`errs.Operational` call site.
