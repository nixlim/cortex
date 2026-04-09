# Feature Specification: Cortex Knowledge System — Phase 1

**Created**: 2026-04-09
**Status**: Approved
**Input**: architecture-v2.md, phase-1-spec-v2.md, research-report.md

---

## User Stories & Acceptance Criteria

### User Story 1 — Authoritative Datom Log and Recovery (Priority: P0)

A developer or AI agent wants every persisted fact to land in one append-only datom log so that Cortex has a single authoritative substrate for replay, merge, retraction, recovery, and time-travel. The current MemPalace split-store design has no durable source of truth, which makes replay and shared-state recovery fragile. This story establishes the substrate every other feature depends on.

**Why this priority**: Without the datom log, there is no authoritative write path, no reliable rebuild story, and no way to recover derived indexes safely.

**Independent Test**: Append datoms, simulate a torn tail and backend drift, then verify startup recovery, self-healing replay, rebuild, merge, and `as-of` all work using only the log.

**Acceptance Scenarios**:

1. **Given** a healthy Cortex installation, **When** a valid write completes, **Then** the system appends a transaction-group of datoms to the writer's active segment file under `~/.cortex/log.d/` using `O_APPEND`, records a SHA-256 checksum per datom, fsyncs once for the transaction group, and treats the fsync return as the commit point.
2. **Given** a prior process crashed while writing the tail of a segment file, **When** any `cortex` command starts, **Then** Cortex validates the last approximately 64 KB of each segment, truncates any torn segment to its last well-formed datom, and appends a `log.recovered` audit datom describing the truncation.
3. **Given** the datom log is ahead of Neo4j or Weaviate watermarks, **When** any `cortex` command starts, **Then** Cortex compares the log `max(tx)` against backend watermarks and replays missing datoms before doing new work.
4. **Given** a populated datom log, **When** `cortex rebuild` runs, **Then** it replays the log into fresh derived state, uses the `embedding_model_name` and `embedding_model_digest` recorded at write time, fails loudly if the pinned model digest is unavailable, and supports `--accept-drift` to explicitly re-embed under the current model with a `model_rebind` audit datom.
5. **Given** two datom logs with overlapping transactions, **When** `cortex merge <log-file>` runs, **Then** the merged result is the set union deduplicated by `tx`.
6. **Given** an asserted entity, **When** it is retracted, **Then** Cortex appends retraction datoms instead of mutating or deleting prior records, hides the entity from default retrieval, and still exposes full lineage through `cortex history <id>`.
7. **Given** lock contention on the datom log, **When** a writer cannot acquire the exclusive advisory lock within 5 seconds, **Then** the command exits with an operational error and writes no partial datoms.
8. **Given** a valid transaction ID from history, **When** `cortex as-of <tx-id> <query>` runs, **Then** only datoms asserted at or before that transaction are visible.
9. **Given** a rebuild where the current installed model digest differs from stored entry digests, **When** `cortex rebuild` runs without `--accept-drift`, **Then** it fails with a pinned-model-drift error listing affected entries; with `--accept-drift`, it re-embeds affected entries and records `model_rebind` datoms.
10. **Given** an entry, frame, trail, subject, or community identifier, **When** an operator runs `cortex retract <id> [--cascade] [--reason=<text>]`, **Then** Cortex appends retraction datoms for the target entity and (with `--cascade`) for any entities whose existence depends on the target (e.g., `DERIVED_FROM` children of a retracted frame), never mutates or deletes prior datoms, records the invoking identity and reason in audit datoms, hides the retracted entity from default retrieval, and keeps full lineage visible via `cortex history <id>` and `cortex as-of`.
11. **Given** a segmented log with multiple segment files, **When** Cortex starts up, **Then** it enumerates all `*.jsonl` files under `~/.cortex/log.d/`, validates the tail of each segment, merge-sorts datoms by `tx`, and produces a single causal stream for replay.
12. **Given** a segment file fails checksum validation on startup, **When** Cortex loads the log, **Then** the corrupt segment is quarantined to `~/.cortex/log.d/.quarantine/` with an entry in `ops.log`, other segments continue to load, and the affected `tx` range is reported via `cortex doctor`.

---

### User Story 2 — Episodic Observation Capture (Priority: P0)

An AI coding agent wants to record observations, hypotheses, decisions, and discoveries as episodic entries with mandatory facets so that useful knowledge becomes durable and queryable immediately. The current workflow has no structured write surface for machine-authored observations, which forces agents to rely on transient context only.

**Why this priority**: Episodic writes are the source material for trails, retrieval, reflection, and cross-project analysis.

**Independent Test**: Run `cortex observe` with valid and invalid inputs, verify datom writes, subject attachment, error handling, and derived-link behavior.

**Acceptance Scenarios**:

1. **Given** an active trail, **When** an agent runs `cortex observe "API has TOCTOU race" --kind=ObservedRace --facets=domain:Security,artifact:Service,project:payment-gw`, **Then** Cortex writes an episodic entry with all required facets, attaches it to the trail, and indexes it in both backends.
2. **Given** an invalid observe request missing a required field or using an unknown kind, **When** `cortex observe` runs, **Then** the command exits with validation code `2`, emits a standard error envelope naming the failing fields, and writes no datoms.
3. **Given** an observation about a canonical subject, **When** `--subject=<psi>` is provided or a subject is resolved from the content, **Then** Cortex creates or reuses the `Subject` entity and writes an `ABOUT` relationship from the entry.
4. **Given** a new observation and existing candidate neighbors, **When** the write pipeline derives links, **Then** Cortex uses the top 5 nearest neighbors from Weaviate, accepts only links with LLM confidence at or above 0.60, applies the additional 0.75 cosine floor for `SIMILAR_TO`, and persists accepted links as datoms.
5. **Given** the link-derivation LLM returns malformed or empty structured output, **When** the observe command is otherwise valid, **Then** the entry write still commits and Cortex records no derived-link datoms for that step.
6. **Given** a `--subject` argument, **When** Cortex validates the PSI, **Then** it enforces the required namespace prefixes (`lib/`, `cve/`, `bugclass/`, `concept/`, `principle/`, `adr/`, `service/`), keeps canonical IDs immutable once minted, and supports alias resolution to canonical PSIs.

---

### User Story 3 — Trail Lifecycle Management (Priority: P0)

An AI agent wants each work session to be captured as a replayable trail so that the sequence of observations and decisions remains recoverable, shareable, and queryable. Without trails, Cortex stores isolated facts but loses the reasoning path that produced them.

**Why this priority**: Trails are a first-class entity in the target architecture and the required integration surface for agents.

**Independent Test**: Begin a trail, write ordered observations, end it, and verify the trail summary, ordering, and filtered listing behavior.

**Acceptance Scenarios**:

1. **Given** no active trail, **When** `cortex trail begin --agent=<name> --name=<label>` runs, **Then** Cortex creates a trail entity, returns its ID, and makes it usable as `CORTEX_TRAIL_ID`.
2. **Given** an active trail with ordered observations, **When** `cortex trail end` runs, **Then** Cortex records `ended_at`, persists an LLM-generated summary, and retains the entry order via `IN_TRAIL.order`.
3. **Given** a completed trail, **When** `cortex trail show <id>` runs, **Then** the result includes name, agent, timestamps, summary, ordered entries, and any derived outcome frames.
4. **Given** many trails, **When** `cortex trail list` runs with filters and optional pagination, **Then** only matching trails are returned in stable order with `--limit` and `--offset` applied.

---

### User Story 4 — Retrieval and Recall Controls (Priority: P0)

An agent wants one recall interface that supports both the default HippoRAG plus ACT-R retrieval path and a small set of alternate retrieval modes so that it can ask for the best answer, explicit traversals, topic summaries, or serendipitous resurfacing without switching systems. Current retrieval is vector-only and loses structure, salience, and context.

**Why this priority**: Retrieval is the primary read path for every other capability.

**Independent Test**: Seed known graph structure and vectors, then verify default recall ranking, alternate-mode behavior, pagination, and reinforcement updates.

**Acceptance Scenarios**:

1. **Given** a populated Cortex graph, **When** `cortex recall "<query>" --json` runs in default mode, **Then** Cortex extracts concepts, resolves seed nodes, runs Personalized PageRank, reranks by ACT-R activation, and returns the top 10 results by default with trail context, community context, and "why surfaced" traces.
2. **Given** returned default-mode results, **When** Cortex finishes assembling the response, **Then** it reinforces each surfaced entity by writing activation-update datoms for `base_activation`, `retrieval_count`, and `last_retrieved_at`.
3. **Given** a valid recall query, **When** `--mode=similar` is used, **Then** Cortex performs pure Weaviate vector similarity retrieval without PPR reranking.
4. **Given** an entry ID and bounded depth, **When** `cortex recall "<query>" --mode=traverse --from=<entry-id> --depth=<n>` runs, **Then** Cortex returns the reachable subgraph from that seed in traversal order with typed edges and no shortest-path compression.
5. **Given** two entity IDs, **When** `cortex path <id-a> <id-b>` or `cortex recall "<query>" --mode=path --from=<id-a> --to=<id-b>` runs, **Then** Cortex returns the shortest graph path between them.
6. **Given** a community ID, **When** `cortex recall "<query>" --mode=community --community=<id> [--level=<n>]` or `cortex community show <id>` runs, **Then** Cortex returns the community summary plus top members for the requested level.
7. **Given** stale knowledge below the default visibility threshold, **When** `--mode=surprise` runs, **Then** Cortex weights retrieval toward under-used or stale items using `(1 - recency)` and may surface otherwise-hidden knowledge.
8. **Given** a large result set, **When** any list-like retrieval command runs with `--limit` and `--offset`, **Then** results are paginated deterministically and `--json` output remains machine-parseable.

---

### User Story 5 — Code Ingestion Pipeline (Priority: P0)

A developer wants to ingest codebases into Cortex so that modules, dependencies, and recurring patterns become episodic observations that participate in the same retrieval and reflection flows as hand-authored observations. Without ingestion, Phase 1 cannot prove its cross-project value on real repositories.

**Why this priority**: Ingestion is the highest-volume write path and the main producer of reusable cross-project signal.

**Independent Test**: Ingest fixture repositories across multiple languages, then verify module grouping, incremental updates, status reporting, and synthesized ingest trails.

**Acceptance Scenarios**:

1. **Given** a local project path, **When** `cortex ingest <path> --project=<name>` runs, **Then** Cortex walks the repository respecting `.gitignore`, include and exclude globs, and the configured language strategy matrix, skips files above 262144 bytes, does not follow symlinks outside the project root, and excludes `~/.cortex/` and paths matching the configurable deny-list, summarizes each module through Ollama, and writes episodic entries through the standard pipeline.
2. **Given** a previously ingested project with a recorded commit SHA, **When** the project changes and ingest runs again, **Then** Cortex reprocesses only touched modules and emits retraction datoms for entries derived from deleted files.
3. **Given** a successful ingest, **When** Cortex finishes, **Then** it writes a synthesized trail `trail:ingest:<project>:<timestamp>` and runs `post_ingest_reflect=true` by default against the ingest time window.
4. **Given** the default configuration, **When** ingest completes, **Then** Cortex does not run cross-project analysis unless `--analyze` is passed or `post_ingest_analyze=true` is configured.
5. **Given** a partial ingest recorded in the log, **When** `cortex ingest resume --project=<name>` runs, **Then** Cortex processes only missing modules and does not duplicate already-written entries.
6. **Given** an ingested project, **When** `cortex ingest status --project=<name>` runs, **Then** Cortex reports the last ingested SHA, timestamp, counts, and whether a partial ingest is resumable.

---

### User Story 6 — Reflection and Frame Consolidation (Priority: P0)

A developer or scheduled job wants Cortex to consolidate episodic observations into reusable semantic or procedural frames so that recurring patterns become structured, comparable knowledge instead of remaining a bag of episodes. Reflection is the only path into the semantic store and encodes the core CLS-inspired split between episodic and semantic memory.

**Why this priority**: Cross-project analysis and high-value retrieval both depend on structured frames, so reflection cannot sit behind the cross-project story.

**Independent Test**: Seed qualifying and non-qualifying episodic clusters, run reflection in normal and dry-run modes, and verify accepted frames, rejected candidates, supersession, contradiction handling, and community refresh.

**Acceptance Scenarios**:

1. **Given** episodic entries recorded since the last successful reflection watermark, **When** `cortex reflect` runs with no explicit `--since`, **Then** Cortex uses the **per-frame** advancement watermark as the default lower bound for candidate selection. Reflection advances the watermark **per accepted frame** (not per run), so that an interrupted reflection that wrote 3 of 5 proposed frames resumes correctly on the next invocation without reprocessing the 3 already-written frames. Scoped reflections (post-ingest, `--contradictions-only`, `--project=`) use a **separate** marker per scope and do **not** advance the global scheduled-reflection watermark.
2. **Given** a candidate cluster, **When** reflection evaluates it, **Then** the cluster qualifies only if it contains at least 3 exemplars, spans at least 2 distinct recording timestamps, meets the 0.65 average pairwise cosine floor, and passes the scheduled MDL ratio of 1.3.
3. **Given** a qualifying cluster, **When** the reflection LLM proposes a valid frame, **Then** Cortex writes a typed frame with slot claims, `DERIVED_FROM` edges, dominant facet metadata, and the frame schema version recorded at assertion time.
4. **Given** a proposed frame that is more general than an existing similar frame, **When** reflection accepts it, **Then** Cortex emits a `SUPERSEDES` edge and leaves the older frame queryable.
5. **Given** a contradiction detected at write time, **When** the contradicted frame is `cross_project=true` or above the 90th percentile by activation, **Then** Cortex triggers scoped re-reflection immediately after the write; otherwise it defers resolution to scheduled reflection while surfacing contradiction markers in retrieval.
6. **Given** `cortex reflect --dry-run --explain`, **When** reflection runs, **Then** Cortex prints candidate clusters, rejection reasons, and proposed frames without writing datoms.
7. **Given** an agent tries to write a semantic or procedural frame directly through `cortex observe`, **When** the command uses a frame-only type, **Then** Cortex rejects the write and explains that reflection is the only write path into the semantic store.
8. **Given** reflection wrote or changed frames, **When** the run completes, **Then** Cortex refreshes only new or changed communities and regenerates summaries for those communities.

---

### User Story 7 — Cross-Project Pattern Analysis (Priority: P0)

A developer wants an unrestricted analysis pass that surfaces reusable patterns only visible across multiple repositories so that Cortex proves its main value proposition: recognizing the same bug class, retry strategy, or design decision across projects. This goes beyond scheduled reflection by deliberately looking across project boundaries.

**Why this priority**: The feature is not considered successful without demonstrating cross-project recall and cross-project frame synthesis on real repositories.

**Independent Test**: Ingest at least two projects sharing a concrete pattern, run `cortex analyze --find-patterns`, and verify that the resulting frame spans both projects and affects retrieval ranking.

**Acceptance Scenarios**:

1. **Given** at least two ingested projects with overlapping signal, **When** `cortex analyze --find-patterns` runs, **Then** Cortex accepts only clusters with exemplars from at least 2 distinct projects, with no more than 70% of exemplars from any single project, and applies the relaxed MDL ratio of 1.15.
2. **Given** accepted cross-project frames, **When** they are written, **Then** Cortex marks them `cross_project=true` and boosts their ACT-R importance weight by +0.20.
3. **Given** `--projects=A,B`, **When** analysis runs, **Then** only entries from those projects participate in candidate selection and resulting `DERIVED_FROM` edges.
4. **Given** `cortex analyze --find-patterns --dry-run --explain`, **When** it runs, **Then** Cortex prints considered clusters, rejection reasons, and proposed frames without writing datoms.
5. **Given** analysis writes frames, **When** the run completes, **Then** Cortex performs a full community detection re-run (Leiden preferred, Louvain fallback) over the semantic graph and refreshes summaries for changed communities.

---

### User Story 8 — Derived Linking, Subject Governance, and Communities (Priority: P1)

A developer wants the substrate to derive links and topic structure automatically so that agents are not forced to author low-signal links manually. Cortex must derive typed local links, govern canonical subjects, and maintain a hierarchical community layer that supports retrieval and browsing.

**Why this priority**: This behavior is central to graph quality, but it depends on the core write, reflection, and analysis paths already being in place.

**Independent Test**: Write entries that share a subject, merge aliases, run community detection, and verify browse commands and non-behaviors.

**Acceptance Scenarios**:

1. **Given** subjects with public or local identities, **When** `cortex subject merge` or `cortex doctor` runs PSI governance checks, **Then** it enforces required prefixes, keeps canonical IDs immutable, supports alias resolution, and detects PSI collisions via conflicting facets.
2. **Given** two PSIs determined to refer to the same subject, **When** `cortex subject merge <psi-a> <psi-b>` runs, **Then** Cortex emits accretive canonical-plus-alias datoms instead of deleting or renaming either subject.
3. **Given** reflection or analysis refreshes communities, **When** `cortex communities [--level=<n>]` or `cortex community show <id>` runs, **Then** Cortex exposes a 3-level hierarchical community structure by default, with summaries and top members at each level.
4. **Given** a user attempts manual link authoring, **When** `cortex link` or `cortex unlink` is invoked, **Then** Cortex rejects the command because links are a derived property, not an authoring verb.

Note: PSI validation rules for `observe` writes are specified in US-2 AS-6.

---

### User Story 9 — Forgetting and Paging Controls (Priority: P1)

An AI agent wants stale knowledge to recede from default retrieval while still remaining recoverable so that Cortex stays useful as the corpus grows. The system also needs explicit paging-style controls (`pin`, `unpin`, `evict`) to let agents manage salience intentionally.

**Why this priority**: Long-term utility depends on principled forgetting, but the core store and retrieval path come first.

**Independent Test**: Simulate decay, explicit pinning, eviction, and reinforcement, then verify visibility changes without any destructive deletion.

**Acceptance Scenarios**:

1. **Given** a newly created entry, **When** Cortex writes it, **Then** it seeds `base_activation=1.0` from the initial encoding event while keeping `retrieval_count=0` until the first actual recall.
2. **Given** an unpinned entry, **When** time passes with no retrievals, **Then** Cortex decays `base_activation` using the configured ACT-R exponent of 0.5 and includes the entry in default retrieval only when `base_activation >= 0.05`.
3. **Given** an entry below the visibility threshold, **When** it is explicitly recalled or surfaced by `--mode=surprise`, **Then** reinforcement can raise it back above the threshold.
4. **Given** `cortex pin <id>` or `cortex unpin <id>`, **When** decay runs, **Then** pinned items resist decay and unpinned items resume normal decay.
5. **Given** `cortex evict <id>`, **When** the command runs, **Then** Cortex forces the entry's `base_activation` to `0.0`, writes a sticky `evicted_at` attribute, and blocks reinforcement from raising the entry above the visibility threshold. The entry remains queryable via `cortex history`, `cortex as-of`, and explicit `cortex get <id>`, but does not appear in default recall, `--mode=similar`, `--mode=traverse`, `--mode=path`, `--mode=community`, or `--mode=surprise` results.
6. **Given** a previously evicted entry, **When** `cortex unevict <id>` runs, **Then** Cortex retracts the `evicted_at` attribute (by writing `evicted_at_retracted`), reinforcement is re-enabled, and subsequent retrievals can restore the entry to visibility. `cortex pin <id>` on an evicted entry has the same effect and additionally marks the entry as pinned.

---

### User Story 10 — Infrastructure and Operational Health (Priority: P0)

A developer wants to start, stop, inspect, and diagnose the local Cortex stack so that write and read commands fail early and visibly when dependencies are misconfigured. Because Phase 1 is local-first and CLI-only, operational health must be expressed through CLI commands instead of a long-running server.

**Why this priority**: No Cortex workflow is usable until the local dependencies are healthy.

**Independent Test**: Start services, inspect status, simulate failures, run doctor, and verify both offline success and actionable diagnostics.

**Acceptance Scenarios**:

1. **Given** Docker is installed, **When** `cortex up` runs, **Then** Cortex starts Weaviate and Neo4j via the configured Compose file, expects Ollama on the host, waits for Weaviate and Neo4j readiness via health endpoints before returning success.
2. **Given** the stack may be partially healthy, **When** `cortex status` runs, **Then** it reports each managed dependency as running, stopped, or degraded without performing the deeper checks reserved for `doctor`.
3. **Given** local services and configuration, **When** `cortex doctor` runs (default `--full` mode), **Then** it checks Docker, Weaviate reachability, Neo4j auth and credential strength, GDS presence with Leiden-preferred/Louvain-fallback detection, Ollama reachability, required Ollama models with digest verification, frame-schema validity, log segment tail recoverability, quarantined segment count, ingest concurrency, pinned-model digest availability for rebuild, file permissions on `~/.cortex/`, disk space for data directories, service endpoint binding, and configured secret-detector ruleset loadability. `cortex doctor --quick` runs only the subset of checks whose execution time is bounded to under 500 ms each (connectivity, file permissions, config parse, segment enumeration) and returns in under 5 seconds total. `cortex doctor --full` has no hard time bound; independent checks run in parallel with a configurable `doctor.parallelism` (default `4`). The doctor command reports each check's status, duration, and remediation guidance.
4. **Given** running containers, **When** `cortex down` runs, **Then** Cortex stops the managed containers cleanly.
5. **Given** the local dependencies are already installed, **When** Cortex commands run without internet access, **Then** observe, recall, reflect, ingest, and analyze continue to work offline.
6. **Given** the Cortex source tree, **When** the release build runs, **Then** it produces a single Go binary targeting macOS ARM64.
7. **Given** a first-time `cortex up`, **When** no Neo4j password has been configured, **Then** Cortex generates a random password, stores it in `~/.cortex/config.yaml` (mode `0600`), and configures the Neo4j container to use it.
8. **Given** `cortex bench [--corpus=small|medium] [--profile=P1-dev|P1-ci]` runs, **Then** it populates a fixture corpus, executes a scripted operation sequence, reports p50/p95/p99 per operation, writes JSON output, and exits `0` on pass or `1` on fail against the profile's envelopes.

The `cortex status --json` output includes per-component status (`healthy`, `degraded`, `down`), version info, log watermark, entry count, and disk usage for data directories.

---

### User Story 11 — MemPalace Migration (Priority: P2)

A developer wants to migrate existing MemPalace data into Cortex so that prior knowledge is preserved and can participate in the new subject, retrieval, and reflection pipelines. This is a one-time bridge, but it preserves existing investment and avoids a cold start.

**Why this priority**: Migration preserves prior knowledge, but it is secondary to making the new Cortex substrate work end to end.

**Independent Test**: Migrate a representative MemPalace export, then verify counts, mappings, and subject unification.

**Acceptance Scenarios**:

1. **Given** a MemPalace export, **When** `cortex migrate --from-mempalace=<path>` runs, **Then** drawers become episodic entries, diary items become `SessionReflection` entries on synthesized trails, and MemPalace entities map to Cortex PSI subjects. Every migrated entry, trail, and frame is marked with the `migrated=true` facet attribute recording the source system and migration timestamp.
2. **Given** migration output, **When** the write pipeline processes migrated data, **Then** subject resolution, derived linking, and post-migration reflection eligibility behave the same as native Cortex writes, and the final report includes created, reused, retracted, and skipped counts.
3. **Given** the `migrated=true` marker on migrated content, **When** `cortex analyze --find-patterns` runs, **Then** migrated entries are **excluded** from cross-project cluster selection by default (because heterogeneous legacy drawer content produces low-quality cross-project signal). The operator can override with `cortex analyze --find-patterns --include-migrated` to explicitly include migrated content in analysis.

---

## Configuration Defaults

### Retrieval and Activation Defaults

| Setting | Default | Meaning |
|---------|---------|---------|
| `retrieval.default_limit` | `10` | Default top-N size for `cortex recall` |
| `retrieval.ppr.seed_top_k` | `5` | Number of nearest neighbors or seed candidates considered for write-time linking and seed resolution |
| `retrieval.ppr.damping` | `0.85` | Personalized PageRank damping factor |
| `retrieval.ppr.max_iterations` | `20` | PPR iteration ceiling |
| `retrieval.activation.decay_exponent` | `0.5` | ACT-R decay exponent |
| `retrieval.activation.weights.base_level` | `0.3` | Weight for ACT-R base level |
| `retrieval.activation.weights.ppr` | `0.3` | Weight for PPR score |
| `retrieval.activation.weights.similarity` | `0.3` | Weight for embedding similarity |
| `retrieval.activation.weights.importance` | `0.1` | Weight for type and facet importance |
| `retrieval.forgetting.visibility_threshold` | `0.05` | Items with `base_activation >= 0.05` appear in default retrieval |
| `pagination.human_default_limit` | `20` | Default page size for human-readable list commands |
| `pagination.json_default_limit` | `100` | Default page size for `--json` list commands |

### Reflection, Analysis, and Communities

| Setting | Default | Meaning |
|---------|---------|---------|
| `link_derivation.confidence_floor` | `0.60` | Minimum LLM confidence for persisting any derived link |
| `link_derivation.similar_to_cosine_floor` | `0.75` | Additional floor for `SIMILAR_TO` |
| `reflection.min_cluster_size` | `3` | Minimum exemplars per cluster |
| `reflection.min_distinct_timestamps` | `2` | Minimum distinct recording events per cluster |
| `reflection.avg_pairwise_cosine_floor` | `0.65` | Coherence floor for candidate clusters |
| `reflection.mdl_compression_ratio` | `1.3` | Scheduled reflection acceptance threshold |
| `analysis.mdl_compression_ratio` | `1.15` | Relaxed threshold for cross-project analysis |
| `analysis.cross_project_min_projects` | `2` | Minimum distinct projects per cross-project cluster |
| `analysis.cross_project_max_share_per_project` | `0.70` | Maximum exemplar share from any one project |
| `analysis.cross_project_importance_boost` | `+0.20` | Additive importance boost during retrieval |
| `community_detection.algorithm` | `leiden` | Community clustering algorithm |
| `community_detection.levels` | `3` | Frozen for Phase 1 (not tunable) |
| `community_detection.resolutions` | `[1.0, 0.5, 0.1]` | Default per-level resolution assumptions for Phase 1 |
| `community_detection.max_iterations` | `10` | Leiden/Louvain convergence cap |
| `community_detection.tolerance` | `0.0001` | Convergence tolerance |

**Community detection validation** (enforced at `cortex up` / `cortex doctor`):
1. `len(resolutions)` must equal `levels`. Mismatch is a hard error.
2. `resolutions` must be strictly decreasing. Level 0 is the most detailed; the last level is the most aggregated.
3. All resolution values must be in the range (0.0, 5.0).

### Ingestion Defaults

| Setting | Default | Meaning |
|---------|---------|---------|
| `ingest.module_size_limit_bytes` | `262144` | Skip oversized files with a warning |
| `ingest.ollama_concurrency` | `4` | Maximum concurrent ingestion workers using Ollama |
| `ingest.post_ingest_reflect` | `true` | Run scoped reflection after ingest |
| `ingest.post_ingest_analyze` | `false` | Do not run cross-project analysis after each ingest unless requested |
| `ingest.default_strategy.go` | `per-package` | Go grouping rule |
| `ingest.default_strategy.java` | `per-class` | Java grouping rule |
| `ingest.default_strategy.kotlin` | `per-file` | Kotlin grouping rule |
| `ingest.default_strategy.python` | `per-file` | Python grouping rule |
| `ingest.default_strategy.javascript_typescript` | `per-file` | JS and TS grouping rule |
| `ingest.default_strategy.rust` | `per-module` | Rust grouping rule |
| `ingest.default_strategy.csharp` | `per-class` | C# grouping rule |
| `ingest.default_strategy.ruby` | `per-class-or-module` | Ruby grouping rule |
| `ingest.default_strategy.c_cpp` | `per-pair` | Header plus implementation grouping rule |
| `ingest.default_strategy.fallback` | `per-file` | Fallback for unknown languages |

Custom ingest strategies via `~/.cortex/ingest/strategies/*.yaml` are deferred to Phase 2. Phase 1 supports the `--strategy` flag to force a built-in strategy and the per-file fallback.

### Operational Defaults

| Setting | Default | Meaning |
|---------|---------|---------|
| `log.lock_timeout_seconds` | `5` | Exclusive per-segment log-lock wait budget |
| `log.tail_validation_window_bytes` | `65536` | Startup per-segment tail recovery read window |
| `log.segment_max_size_mb` | `64` | Segment file size cap before rolling |
| `log.segment_dir` | `~/.cortex/log.d` | Segment directory path (must exist with mode `0700`) |
| `doctor.parallelism` | `4` | Parallel check workers for `cortex doctor --full` |
| `doctor.quick_timeout_seconds` | `5` | Total time budget for `cortex doctor --quick` |
| `security.secrets.builtin_ruleset` | embedded | Built-in regex ruleset at `cortex/security/secrets/builtin.yaml` |
| `security.secrets.custom_ruleset_path` | `~/.cortex/secrets.yaml` | Operator-extension ruleset path (optional) |
| `security.secrets.entropy_threshold` | `4.5` | Shannon entropy floor for generic high-entropy rules |
| `migration.exclude_from_cross_project` | `true` | Exclude migrated entries from `cortex analyze` by default |
| `timeouts.embedding_seconds` | `30` | Timeout for embedding requests |
| `timeouts.concept_extraction_seconds` | `5` | Timeout for recall concept extraction |
| `timeouts.link_derivation_seconds` | `60` | Timeout for A-MEM link derivation |
| `timeouts.trail_summary_seconds` | `60` | Timeout for trail summaries |
| `timeouts.reflection_seconds` | `60` | Timeout for frame proposal or community summary generation |
| `timeouts.ingest_summary_seconds` | `120` | Timeout for per-module ingest summarization |
| `cli.exit_code.success` | `0` | Successful completion |
| `cli.exit_code.operational` | `1` | Runtime, dependency, timeout, or not-found failure |
| `cli.exit_code.validation` | `2` | Validation or usage failure |
| `endpoints.weaviate_http` | `localhost:9397` | Weaviate HTTP endpoint (configurable) |
| `endpoints.weaviate_grpc` | `localhost:50051` | Weaviate gRPC endpoint (configurable) |
| `endpoints.neo4j_bolt` | `localhost:7687` | Neo4j Bolt endpoint (configurable) |
| `endpoints.ollama` | `localhost:11434` | Ollama HTTP endpoint (configurable) |
| `ops_log.format` | `jsonl` | Structured JSON Lines |
| `ops_log.max_size_mb` | `50` | Rotate to ops.log.1 at this size |
| `ops_log.fields` | see below | Required: timestamp, level, invocation_id (ULID), component, tx (if applicable), message, error (if applicable) |
| `security.file_mode_directory` | `0700` | Owner-only directory permissions for `~/.cortex/` |
| `security.file_mode_files` | `0600` | Owner-only file permissions for all files in `~/.cortex/` |
| `disk.warning_threshold_gb` | `1` | `cortex doctor` warns when free disk is below this |

### Logging

`~/.cortex/ops.log` uses structured JSON Lines format. Each line contains:
- `timestamp`: ISO 8601
- `level`: `DEBUG`, `INFO`, `WARN`, `ERROR`
- `invocation_id`: ULID generated at command start, shared by all log entries within a single `cortex` invocation
- `component`: subsystem name (e.g., `log`, `weaviate`, `neo4j`, `ollama`, `reflect`, `analyze`)
- `tx`: transaction ID, if applicable
- `entity_ids`: affected entity IDs, if applicable
- `message`: human-readable description
- `error`: error details (structured, never raw backend messages or stack traces)

**stderr** receives a human-readable summary; **ops.log** receives the structured detail. `cortex doctor` reads `ops.log` for recent error patterns and timeout frequency.

Log rotation: when `ops.log` exceeds `ops_log.max_size_mb` (default 50 MB), rotate to `ops.log.1`.

---

## Frame Type Registry (Phase 1)

Phase 1 ships eleven built-in frame types as versioned, stable definitions in `cortex/frames/builtin/*.json` embedded in the binary. Agents cannot add or redefine built-in frames at runtime; operators may add custom frames via `~/.cortex/frames/*.json`, which are validated at `cortex up` / `cortex doctor`. Changing a built-in frame's slot set requires incrementing its `version` field; entries continue to carry the version under which they were asserted.

| Frame Type | Store | Required Slots | Optional Slots | Notes |
|---|---|---|---|---|
| `Observation` | Episodic | `body` | `source_ref`, `subject` | The default episodic kind; what agents write during work. |
| `SessionReflection` | Episodic | `body`, `agent` | `outcomes` | Replaces MemPalace diaries. One per `cortex trail end`. |
| `ObservedRace` | Episodic | `body`, `participants`, `ordering_violation` | `reproducer` | Episodic because races are observed events, not consolidated patterns. |
| `BugPattern` | Semantic | `name`, `symptom`, `root_cause`, `conditions`, `remediation` | `exemplars`, `first_seen`, `last_seen` | Classic reusable bug archetype. Reflection-only. |
| `DesignDecision` | Procedural | `name`, `context`, `choice`, `rationale`, `consequences` | `alternatives`, `supersedes` | ADR-shaped. Reflection-only. |
| `RetryPattern` | Procedural | `name`, `trigger`, `strategy`, `backoff`, `max_attempts`, `terminal_error_handling` | `jitter`, `idempotency_note` | High-value cross-project target. Reflection-only. |
| `ReliabilityPattern` | Procedural | `name`, `failure_mode_addressed`, `technique`, `scope` | `tradeoffs` | Circuit breakers, timeouts, bulkheads. Reflection-only. |
| `SecurityPattern` | Procedural | `name`, `threat_model`, `mitigation`, `residual_risk` | `references` | AuthN/Z, crypto, input handling. Reflection-only. |
| `LibraryBehavior` | Semantic | `library_psi`, `behavior`, `observed_in` | `version_range`, `workaround` | Anchored on a `lib/...` PSI subject. Reflection-only. |
| `Principle` | Procedural | `name`, `statement`, `scope` | `counterexamples` | Cross-cutting architectural principle. Reflection-only. |
| `ArchitectureNote` | Semantic | `subject_psi`, `body` | `diagram_ref` | Freeform architectural observation anchored to a subject. Reflection-only. |

**Epistemic boundaries**:
- Agents may write `Observation`, `SessionReflection`, and `ObservedRace` directly via `cortex observe`.
- The eight reflection-only frame types are populated **exclusively** by `cortex reflect` and `cortex analyze --find-patterns`. An agent attempting `cortex observe --kind=<reflection-only-type>` is rejected with a validation error pointing at the consolidation pipeline.

**Custom frames**: `~/.cortex/frames/*.json` may declare new frame types with any name, required slots, optional slots, and store assignment (`episodic`, `semantic`, or `procedural`). Custom frame files are loaded and validated at startup; invalid files fail `cortex up` loudly. Custom frames are **never** permitted to redefine or override built-in types.

---

## Benchmarking

### Hardware Profiles

| Profile | Role | Specification |
|---------|------|--------------|
| P1-dev | Authoritative for all latency envelopes | Apple Silicon (M-series), 16 GB unified memory, NVMe SSD, Ollama + Weaviate + Neo4j local |
| P1-ci | Regression detection only, not authoritative | GitHub Actions macos-14 or equivalent; envelopes get a 2x multiplier |

CI results detect regressions relative to previous CI runs on the same runner. They are never used to validate an envelope.

### Corpus Sizes

| Corpus | Entries | Frames | Subjects | Read Envelope (p95) | Write Envelope (p95) |
|--------|---------|--------|----------|---------------------|----------------------|
| small | 1,000 | 100 | 50 | < 2.0 s | < 3.0 s |
| medium | 10,000 | 1,000 | 500 | < 3.5 s | < 5.0 s |

**Note**: The recall read envelope measures the **full** default recall pipeline including LLM concept extraction. Concept extraction alone typically accounts for 300-1500 ms on local Apple Silicon.

### Stack State

- **Warm stack only**: models loaded, backends running, no cold-start included.
- **Quiesced**: no background reflection, analyze, or ingest running during benchmarking.

### `cortex bench`

```
cortex bench [--corpus=small|medium] [--profile=P1-dev|P1-ci] [--output=<path>]
```

1. Populates (or reuses) a fixture corpus at `~/.cortex/bench/fixtures/<corpus>/`.
2. Runs a scripted sequence: 200 recall calls, 200 observe calls, 20 `reflect --dry-run` calls, 5 `analyze --dry-run` calls.
3. Reports p50/p95/p99 per operation. Writes JSON to `~/.cortex/bench/latest.json`.
4. Compares against the profile's envelope table. Returns exit `0` on pass, `1` on fail.

---

## Infrastructure Topology

This section pins the middle layer between the hardware profile (Benchmarking) and the endpoint configuration (Configuration Defaults). Its purpose is to answer one question: *if two operators build and run Phase 1 on identical hardware, will they reach consistent derived state?* Without pinned container versions, a declared resource allocation, a concrete readiness contract, and a volume topology the spec cannot guarantee that answer. The sub-sections below close that gap.

The literal `docker-compose.yaml`, Neo4j `neo4j.conf`, and the custom Neo4j image `Dockerfile` live in the Cortex repository under `docker/` as code, not as spec prose. This section specifies *what* those files must declare; the repository holds *how*. Drift between this section and the files under `docker/` is a spec bug and should be resolved in favor of this section.

### Pinned Service Versions

| Service | Image | Pinned Version | Source | Notes |
|---|---|---|---|---|
| Weaviate | `cr.weaviate.io/semitechnologies/weaviate` | `1.36.9` | upstream | Referenced directly; Cortex does not re-distribute. |
| Neo4j + GDS | `cortex/neo4j-gds` | `<cortex-release-version>` | **built locally** from `docker/neo4j-gds/Dockerfile` | Custom image; see *Custom Neo4j + GDS Image* below. |
| Ollama | host-installed binary | `>= 0.1.40` | host | Not containerized. Runs as a host service. |
| Embedding model | `nomic-embed-text` | digest pinned at install time | Ollama registry | `cortex up` pulls on first run and records the digest in `~/.cortex/config.yaml`. |
| Generation model | `llama3.1:8b-instruct` | digest pinned at install time | Ollama registry | Used for concept extraction, trail summaries, link derivation, reflection, community summaries. |

**Version bumping policy for Phase 1.** Pinned image versions are part of the release artifact. Bumping Weaviate, the Neo4j base, or the GDS plugin requires a new Cortex release, not a config change. The pinned image tag MUST appear in `docker/neo4j-gds/Dockerfile`, in the generated `docker-compose.yaml`, and in the output of `cortex version --infra`.

**Model digest pinning.** The spec does not hard-code an Ollama model digest because the digest is determined at install time by which content Ollama pulled. Instead, `cortex up` records the digest once on first pull and treats that recorded digest as authoritative for all subsequent writes and rebuilds (see FR-051). Two operators who install on the same day will typically end up with matching digests; an operator who re-pulls `latest` months later will get a new digest that fails `cortex rebuild` without `--accept-drift`. This is the intended behavior — it makes model drift visible instead of silent.

### Custom Neo4j + GDS Image

Phase 1 ships a custom Neo4j image with Graph Data Science pre-baked. This avoids the fragile "install GDS at first boot" path, makes `cortex doctor` deterministic, and sidesteps any timing races between Neo4j startup and plugin loading.

**Build topology:**
- Dockerfile lives at `docker/neo4j-gds/Dockerfile` in the Cortex repository.
- Base image: `neo4j:5.24-community` (pinned).
- GDS plugin: Neo4j Graph Data Science community edition, pinned to a specific release in the Dockerfile. GDS is downloaded from Neo4j's official plugin distribution at `docker build` time, placed in `/var/lib/neo4j/plugins/`, and chown'd. The Neo4j configuration enables the plugin via `dbms.security.procedures.unrestricted=gds.*` and `dbms.security.procedures.allowlist=gds.*`.
- Image tag: `cortex/neo4j-gds:<cortex-release-version>` — the tag is bound to the Cortex release, not to Neo4j or GDS versions, so `cortex` and its Neo4j image upgrade as a unit.
- Health-check baked in: the image declares a Docker `HEALTHCHECK` that runs a Cypher ping (`RETURN 1`) via `cypher-shell` and exits non-zero until the database accepts queries.

**Build and distribution:**
- `cortex up` looks for the `cortex/neo4j-gds:<cortex-release-version>` tag in the local Docker daemon.
- If missing, `cortex up` runs `docker build` against the Dockerfile shipped with the release (located alongside the Cortex binary or at a path referenced by `docker.neo4j_gds_dockerfile` in `config.yaml`).
- Cortex does **not** push a pre-built image to any public registry. Distribution is source (`Dockerfile` + GDS download URL) only. This keeps Phase 1 compliant with Neo4j + GDS licensing intent: Cortex re-distributes no binaries; each operator's machine builds its own image from the upstream sources.
- After a successful build, the resulting image is cached in the local Docker daemon. Subsequent `cortex up` invocations are instant.
- Offline build: the Dockerfile supports an `--build-arg GDS_LOCAL_PATH=/path/to/gds.jar` so operators without internet can point the build at a pre-downloaded GDS jar.

**GDS version detection.** `cortex doctor` queries `gds.version()` after Neo4j is up and records the result alongside the procedure availability checks. The Leiden-preferred / Louvain-fallback logic (FR-028) is keyed off the actual callable procedures, not the version string, so a GDS upgrade that moves procedures in or out of the community edition is handled transparently.

### Resource Allocation

Per-service memory and concurrency limits that the `docker-compose.yaml` MUST declare. These are the minimums required for SC-006 performance envelopes to be achievable on the P1-dev hardware profile.

| Service | RAM limit | Disk floor | Other |
|---|---|---|---|
| Weaviate | `2G` request, `4G` limit | `10G` on the Weaviate volume | `LIMIT_RESOURCES=true` enabled |
| Neo4j + GDS | `2G` request, `4G` limit | `10G` on the Neo4j volume | JVM heap `-Xms1G -Xmx2G`; page cache `1G`; GDS memory budget `1G` |
| Ollama (host) | no container limit | model-dependent (`nomic-embed-text` ~300 MB, `llama3.1:8b-instruct` ~5 GB) | Host-managed; `cortex doctor` checks installed model count and reports if the host has <8 GB free RAM when generation-model ops are expected |

Total container footprint: **4 GB request / 8 GB limit** for the two managed services. With Ollama loaded, a 16 GB developer machine is the minimum viable workstation. `cortex doctor` fails loudly if the host has less than 12 GB total RAM.

**Why these allocations matter for SC-006.** The benchmark envelope (<2 s p95 recall on the small corpus) assumes the HNSW index fits in Weaviate RAM and the working set of the semantic graph fits in Neo4j's page cache. Under-allocation produces envelope failures that look like Cortex bugs but are actually container starvation. If an operator cannot meet the RAM floor, `cortex bench` fails with a diagnostic pointing at `cortex doctor` instead of at a PPR regression.

**Tuning posture.** Phase 1 does not expose per-service resource tuning as a supported configuration surface. Operators who need to change the allocations edit `docker/docker-compose.yaml` directly and accept that their deployment is no longer spec-compliant. Phase 2 may expose a tuning surface if real-world usage demands it.

### `cortex up` Readiness Contract

`cortex up` is not considered successful until every managed service has passed an explicit readiness probe. The contract:

1. **Docker daemon reachability.** Before any service start, `cortex up` pings the Docker daemon and fails with `DOCKER_UNREACHABLE` (exit `1`) if it is not running.
2. **Build the custom Neo4j image if absent** (see *Custom Neo4j + GDS Image*).
3. **Start services in dependency order**: Weaviate first, then Neo4j. Ollama is a host service and is not started by `cortex up`; its presence is checked, not managed.
4. **Per-service readiness probes**:
   - **Weaviate**: HTTP `GET /v1/.well-known/ready` returns 200. Probe budget: 30 seconds from container start. Retry interval: 1 second.
   - **Neo4j**: open a Bolt connection, authenticate with the credential stored in `~/.cortex/config.yaml`, and run `RETURN 1`. Probe budget: 60 seconds from container start (Neo4j is slow to boot). Retry interval: 2 seconds.
   - **GDS plugin**: immediately after the Bolt ping succeeds, run `CALL gds.version()` and require a successful result. If the call fails, `cortex up` exits with `GDS_NOT_AVAILABLE` and does not consider Neo4j ready.
   - **Ollama**: HTTP `GET http://localhost:11434/api/tags` returns 200 and the response contains each configured model name. If the embedding model is absent, `cortex up` exits with `OLLAMA_MODEL_MISSING` and prints the exact `ollama pull` command the operator should run.
5. **Overall startup budget**: 90 seconds wall-clock from `cortex up` invocation. If either service fails its probe within its per-service budget but total elapsed time is still under 90 seconds, the command waits for both. If total elapsed time exceeds 90 seconds with any service still not ready, `cortex up` exits with `STARTUP_BUDGET_EXCEEDED` (exit `1`) and leaves the containers running so `cortex status` and `cortex doctor` can diagnose.
6. **Partial success posture**: `cortex up` does **not** consider a partial start a success. If Weaviate is ready and Neo4j is not (or vice versa), the command exits non-zero. A degraded mode where one backend is unavailable is handled by `cortex status`, `cortex doctor`, and the partial-backend-availability behavior described in the Behavioral Contract — but `cortex up` itself is all-or-nothing.

### Volume Topology and Persistence

Managed services persist state in named Docker volumes owned by Cortex. Volume names are deterministic and bound to the Cortex release.

| Volume | Mount path (container) | Contents | Survives `cortex down` | Survives `cortex up` re-run |
|---|---|---|---|---|
| `cortex_weaviate_data` | `/var/lib/weaviate` | Weaviate object store and HNSW indexes | Yes | Yes |
| `cortex_neo4j_data` | `/data` | Neo4j graph store, GDS state | Yes | Yes |

**Volume lifecycle.**
- `cortex up` creates the volumes on first invocation if they do not exist.
- `cortex down` stops the containers but does **not** remove volumes. Running `cortex up` after `cortex down` resumes against the same data.
- `cortex down --purge` removes the volumes after operator confirmation. This is the only in-band destructive operation on the managed-service tier. The datom log in `~/.cortex/log.d/` is untouched by `--purge`; rebuild from log is the recovery path.
- `cortex doctor` reports per-volume disk usage and warns when free space falls below `disk.warning_threshold_gb`.

**Host path vs named volumes.** Phase 1 uses named Docker volumes, not host bind mounts. Named volumes are portable across Docker daemon upgrades on the same host, survive a Docker Desktop restart, and do not interact with host file permissions in surprising ways. Operators who want a host path (e.g., for ZFS snapshots) edit `docker-compose.yaml` and accept the deviation from spec-compliant deployment.

**Backup implications.** The datom log at `~/.cortex/log.d/` is authoritative. Weaviate and Neo4j volumes are derived and disposable — they can be rebuilt at any time from the log via `cortex rebuild`. The supported backup path is therefore:
- **Primary**: `cortex export` (or `rsync -a ~/.cortex/log.d/`) for the datom log.
- **Not required**: backing up the Weaviate / Neo4j volumes. They can be regenerated from the log.

Operators may snapshot the volumes for faster restore, but the spec does not require it, and corruption of a snapshot does not threaten data because the log is always the source of truth. This resolves the concern raised in Implementer Note 2: the backup unit is the log directory, not the service volumes.

### Upgrade Posture

Phase 1 has no in-place upgrade path for managed services. The supported upgrade workflow is:

1. `cortex export --output=/path/to/backup.jsonl` (or `rsync -a ~/.cortex/log.d/ /backup/log.d/`).
2. `cortex down --purge` (removes the volumes of the old version).
3. Install the new Cortex release.
4. `cortex up` (builds the new custom Neo4j + GDS image if needed).
5. `cortex rebuild` (replays the datom log into the fresh derived indexes under the new service versions).

This is a full rebuild, not a rolling upgrade. For Phase 1 local-operator scope it is acceptable because (a) the datom log is small enough to replay in minutes for realistic corpus sizes, and (b) Cortex has no uptime requirement — a 15-minute downtime during upgrade is fine. Phase 2 may introduce a smoother upgrade path if needed. The upgrade posture is explicitly a trade-off in favor of simplicity and safety.

### Host Prerequisites

Checked by `cortex doctor` and enforced before `cortex up` will start any service.

| Requirement | Minimum | Recommended | Check |
|---|---|---|---|
| macOS | `darwin/arm64` | any currently-supported version | `uname -s && uname -m` |
| Docker Desktop or equivalent | `>= 4.25` with Compose v2 | latest stable | `docker version` |
| Total host RAM | `12 GB` | `16 GB` or more | `sysctl hw.memsize` |
| Free disk on `~/.cortex` volume | `10 GB` | `50 GB` | `df -g ~/.cortex` |
| Open ports | `9397`, `50051`, `7474`, `7687` | same | socket probe |
| File descriptor limit | `ulimit -n >= 4096` | `8192` | `ulimit -n` |
| Ollama installed | `>= 0.1.40`, loopback bind | latest | `ollama --version && curl -s http://localhost:11434/api/tags` |
| Models pulled | `nomic-embed-text`, `llama3.1:8b-instruct` | same | `ollama list` |

Any failed check produces a distinct `cortex doctor` error with exact remediation (e.g., `Run: ulimit -n 8192`, `Run: ollama pull nomic-embed-text`). `cortex up` refuses to start services if any hard-fail prerequisite is unmet.

### Behavioral Requirements Added by This Section

- **FR-057**: System MUST build or locate the `cortex/neo4j-gds:<cortex-release-version>` image locally on `cortex up` and MUST NOT pull it from any remote registry. The Dockerfile ships in the Cortex repository under `docker/neo4j-gds/`.
- **FR-058**: System MUST enforce the `cortex up` readiness contract in full — Docker daemon check, ordered service start, per-service probes, GDS version call, Ollama model presence, 90-second total budget — and MUST exit non-zero on partial success.
- **FR-059**: System MUST declare per-service resource requests and limits in the generated `docker-compose.yaml` matching the Resource Allocation table above, and MUST fail `cortex doctor` if the host has less than 12 GB total RAM.
- **FR-060**: System MUST use named Docker volumes (`cortex_weaviate_data`, `cortex_neo4j_data`) for managed services, MUST preserve them across `cortex down` and `cortex up`, and MUST provide `cortex down --purge` as the only in-band destructive operation against managed-service storage.
- **FR-061**: System MUST NOT provide an in-place upgrade path for managed services in Phase 1. Upgrades MUST go through the documented `export → down --purge → up → rebuild` sequence.
- **FR-062**: System MUST verify all Host Prerequisites in `cortex doctor` and MUST block `cortex up` on hard-fail prerequisites (Docker unreachable, RAM under floor, required ports in use, Ollama missing, required models absent).

These requirements are cross-referenced in the Traceability Matrix below.

---

## Concurrency and Durability

### Segmented Datom Log

The datom log is physically organized as a directory of append-only segment files at `~/.cortex/log.d/`. Every writer opens or creates its own segment file per invocation and appends to it exclusively. Segment file names follow the pattern `<ulid>-<writer-id>.jsonl`, where the ULID prefix encodes the segment's creation time and guarantees lexicographic ordering of segment files roughly matches temporal order.

- **Reads** (replay, rebuild, recall-time consistency check) enumerate all `*.jsonl` segment files, parse each line, and merge-sort by datom `tx` ULID.
- **Writes** (`cortex observe`, `cortex ingest`, etc.) append to a single current segment owned by the invoking process. Lock scope (below) is per-segment, not global.
- **Merges** (`cortex merge <external-log>`) drop the foreign segment files into `log.d/` verbatim after checksum validation. No rewrite of existing segments.
- **Segment size cap**: `log.segment_max_size_mb` (default `64`). When a writer's current segment exceeds the cap, it finalizes the segment (fsync, close) and starts a new one. Segment finalization is not a separate command; it happens transparently during normal writes.
- **Segment directory permissions**: `0700`, same as the rest of `~/.cortex/`.

The `tx` ULID within a datom is the causal ordering key and is authoritative regardless of which segment file a datom lives in.

### Lock Scope

The advisory `flock(LOCK_EX)` covers **only** the current segment file's append and `fsync()`. Lock is per-segment, not global. Writers working on different segment files do not contend; the only contention is between a writer and a reader replaying the same segment, which is handled by the checksum tail-validation protocol at startup.

| Operation | Under Lock | Outside Lock |
|-----------|-----------|-------------|
| Serialize datom group | | Y |
| Append to current segment in `~/.cortex/log.d/` | Y | |
| `fsync()` (commit point) | Y | |
| Release lock | Y | |
| Weaviate write | | Y |
| Neo4j write | | Y |
| Watermark update (`:CortexMeta` / `cortex_meta`) | | Y |
| LLM link derivation | | Y |
| LLM PSI resolution | | Y |
| LLM frame generation (reflection) | | Y |
| Segment finalization (close + rename + fsync dir) | Y | |
| `cortex merge` segment file drop-in | | Y (by rename after checksum validation) |
| `cortex merge` re-indexing | | Y |

### Transaction Ordering

Log-file append order and ULID sort order may differ under concurrent writes and after merges that pull in external segments. Backends apply datoms in **ULID sort order** for causal consistency, not segment-file order. Readers performing startup replay MUST merge-sort all segment files by datom `tx` before applying to backends.

### Activation Datom Replay Semantics (Last-Write-Wins)

Reinforcement datoms written by `cortex recall` (`base_activation`, `retrieval_count`, `last_retrieved_at` updates) use **last-write-wins replay semantics**. During rebuild or self-healing replay, only the activation datom with the highest `tx` for a given `(entity, attribute)` pair is applied to the derived indexes; older activation datoms for the same attribute are no-ops for backend state.

Consequences:
- The log remains pure append-only.
- Rebuild cost is proportional to the number of distinct `(entity, attribute)` activation pairs, not the total number of activation writes over history.
- `cortex history <id>` still returns the full activation lineage, because history queries read the log directly and do not collapse LWW datoms.
- Non-activation datoms (knowledge, links, frames, trails, facets, PSI subjects, communities) retain full event semantics and are not subject to LWW collapse.

LWW is declared per attribute in the datom-log schema. Attributes marked LWW in Phase 1 are: `base_activation`, `retrieval_count`, `last_retrieved_at`, and the sticky `evicted_at` / `evicted_at_retracted` pair defined in US-9.

### Read-Dependent Commands

The following commands MUST run the startup tail-replay consistency check (enumerate segment files, validate tails, compare merged `max(tx)` against backend watermarks, replay missing datoms) before doing any read-dependent work:

`recall`, `reflect`, `analyze`, `subject merge`, `communities`, `community show`, `path`, `trail show`, `trail list`, `history`, `as-of`

### Advisory Lock Limitation

Advisory locking assumes all writers are cooperative Cortex processes. Non-cooperative writes are detectable via checksum validation at startup but not preventable without mandatory locking or a daemon. This is an accepted limitation for the Phase 1 single-operator scope.

---

## ACT-R Activation Formula

The composite activation score used for default recall reranking is:

```
activation(e, q) = w_base * B(e) + w_ppr * PPR(e) + w_sim * sim(e, q) + w_imp * I(e)
```

Where:
- **B(e)** = ln(sum_j t_j^{-d}) -- ACT-R base-level activation. Each t_j is the elapsed time in seconds since the j-th retrieval (or the initial encoding event). d = `retrieval.activation.decay_exponent` (default 0.5).
- **PPR(e)** = Personalized PageRank score from the seed set resolved for the current query.
- **sim(e, q)** = cosine similarity between the entry's embedding and the query embedding.
- **I(e)** = importance term: 0.0 base, +0.20 if `cross_project=true`, plus any type-based or facet-based importance adjustments.
- Weights: `w_base` = 0.3, `w_ppr` = 0.3, `w_sim` = 0.3, `w_imp` = 0.1 (from `retrieval.activation.weights.*`).

The `base_activation` field stored on each entry is the current value of B(e), updated on every retrieval reinforcement event. Entries with `base_activation < visibility_threshold` (default 0.05, inclusive: `>=`) are excluded from default retrieval but remain queryable via `--mode=surprise`, `cortex history`, and `cortex as-of`.

---

## Behavioral Contract

Primary flows:
- When any `cortex` command starts, the system first validates the tail of the datom log and then compares the log watermark to backend watermarks, repairing torn tails and replaying any missing committed datoms before other work begins.
- When `cortex observe` runs with valid input, the system validates the payload, sanitizes user-provided content for LLM prompt construction (treating observation bodies as opaque data with structured prompt templates and clear delimiters), resolves any PSI subject, computes the embedding, **acquires the log lock**, appends and fsyncs the final entry datom group, **releases the lock**, then **outside the lock**: applies the committed datoms to Neo4j and Weaviate, derives links via LLM, updates backend watermarks, and returns the new entry ID.
- When `cortex trail end` runs, the system reads the ordered trail entries, generates a trail summary, appends summary datoms, and marks the trail as ended.
- When `cortex recall` runs **in default mode**, the system extracts concepts, resolves seeds, runs PPR, reranks by ACT-R activation (see ACT-R Activation Formula), paginates the final result set, returns trail and community context with 'why surfaced' traces, and then writes reinforcement datoms for surfaced results. **Alternate modes** (`similar`, `traverse`, `path`, `community`, `surprise`) use different pipelines as specified in their BDD scenarios.
- When `cortex reflect` runs, the system selects candidate episodic clusters from the requested time window, evaluates threshold rules, proposes frames, writes accepted frames with provenance, and refreshes changed communities.
- When `cortex analyze --find-patterns` runs, the system applies the cross-project thresholds, writes only accepted cross-project frames, and performs a full community refresh.
- When `cortex ingest` runs, the system walks eligible files, groups modules by strategy, summarizes modules, writes entries through the standard write path, synthesizes an ingest trail, records ingest state, and optionally triggers post-ingest reflection or analysis based on configuration and flags.
- When a long-running command (`reflect`, `analyze`, `ingest`) receives SIGINT or SIGTERM, the system completes or discards the current transaction group, updates watermarks, and exits. Partial results are either fully committed or fully discarded at the transaction-group granularity.

Error flows:
- When input validation fails, the system returns exit code `2` and emits a standard JSON envelope shaped as `{"error":{"code":"<CODE>","message":"<message>","details":{...}}}` when `--json` is requested.
- When the log lock cannot be acquired within 5 seconds, the system returns exit code `1`, writes no partial datoms, and leaves the log unchanged.
- When an Ollama call times out or a required model is unavailable before the commit point, the system aborts the operation and writes no transaction-group datoms for that operation.
- When the datom log commit succeeds but backend apply or watermark update fails afterward, the system records the operational failure to stderr and `~/.cortex/ops.log`, returns success for the committed write, and relies on startup self-healing replay on the next invocation.
- When `cortex rebuild` detects that the current installed model digest differs from entries' stored `embedding_model_digest`, the system fails with a pinned-model-drift error listing affected entries. The operator can install the pinned digest or pass `--accept-drift` to re-embed under the current model with `model_rebind` audit datoms.
- When `cortex doctor` finds missing services, invalid frame schemas, missing models, or GDS absence, the system reports each failing check separately and includes remediation guidance.
- When error envelopes are returned in `--json` mode, the system MUST NOT include raw backend error messages, stack traces, or internal file paths. Raw details are logged to `ops.log` only.
- When any timeout fires (embedding, link derivation, reflection, etc.), the system logs the event to `ops.log` with operation type, elapsed time, and timeout budget.
- When `cortex doctor` detects GDS is installed but `gds.leiden.stream` is unavailable, the system warns and falls back to Louvain automatically for community detection. Missing `gds.pageRank.stream` is a hard error.

Boundary conditions:
- When the datom log is empty, `cortex recall` returns an empty result set with exit code `0`.
- When the graph has fewer than the configured minimum community size, community refresh produces no communities and no error.
- When a new entry has no viable nearest neighbors, A-MEM linking produces zero derived links and the write still succeeds.
- When `base_activation` is exactly `0.05`, the entry remains eligible for default retrieval because the visibility threshold is inclusive (`base_activation >= 0.05`).
- When `cortex evict` runs, it forces `base_activation` to `0.0`, which is strictly below the inclusive visibility threshold.
- When `cortex ingest resume` targets a completed ingest, the command returns a no-op status message and writes no new datoms.
- When `cortex as-of` references a transaction ID not present in the log, the command returns exit code `1` with a clear not-found error.

**Partial Backend Availability**: When one backend is down, commands that depend only on the other may still function. `--mode=similar` requires Weaviate only. `--mode=traverse`, `--mode=path`, and `--mode=community` require Neo4j only. Default recall requires both. `cortex observe` commits to the log regardless; backend apply is retried via self-healing. A full degradation matrix is deferred to Phase 2; Phase 1 requires all backends healthy for full functionality.

---

## Edge Cases

- What happens when two writers hit the datom log concurrently? Expected: the advisory `flock` serializes only the log append and fsync; after lock release, both writers proceed with backend apply and LLM derivation in parallel. Backends apply in ULID sort order. The waiting writer either succeeds after the first writer releases the lock or exits after 5 seconds with no corruption.
- What happens when the log tail contains a partial JSON line with an invalid checksum? Expected: startup recovery truncates to the last valid datom, appends a `log.recovered` audit datom, and continues.
- What happens when a committed write updates the log but not one backend? Expected: the write remains committed, backend drift is detected on the next command, and missing datoms are replayed automatically before the next command proceeds.
- What happens when `cortex rebuild` is interrupted? Expected: rebuild clears derived state into a staging namespace at the start, so interruption leaves the prior active index untouched. A subsequent rebuild can be rerun safely from scratch. Rebuild is idempotent.
- What happens when a derived-link LLM returns malformed JSON? Expected: the entry write succeeds and only the derived-link portion is skipped.
- What happens when ingestion encounters a file above `module_size_limit_bytes`? Expected: Cortex skips the file, records a warning, and continues processing other modules.
- What happens when a PSI merge would unify contradictory claims? Expected: both claims remain with provenance, contradiction edges may be emitted, and neither claim is deleted.
- What happens when a community loses all members above the visibility threshold? Expected: the community is omitted from default community listings but remains in history and can be recomputed later.
- What happens when a contradiction is written against a highly important frame? Expected: Cortex immediately queues a scoped re-reflection for that cluster instead of waiting for the full schedule.
- What happens when `cortex status` runs while one backend is healthy and another is not? Expected: status marks the unhealthy service degraded without running the deeper doctor checks.
- What happens when an operator sends SIGINT during `cortex reflect`? Expected: the current transaction group is completed or discarded, watermarks are consistent, and a subsequent reflect can resume safely.
- What happens when `cortex ingest` encounters a symlink pointing outside the project root? Expected: the symlink is skipped with a warning; Cortex does not follow symlinks outside the project boundary by default.
- What happens when observation body text contains prompt-injection attempts? Expected: LLM prompt construction uses structured templates with clear delimiters; user-provided content is treated as opaque data, not interpolated raw into prompts.
- What happens when the corpus is below the minimum size required for meaningful community detection (e.g., the small benchmark corpus or a fresh install)? Expected: Leiden/Louvain runs and produces zero or one trivial community; `cortex communities` returns an empty list with exit `0`; community-dependent retrieval modes (`--mode=community`) return empty result sets with exit `0`; reflection does not fail.
- What happens when `cortex compact`-style operations are needed? Expected: not required in Phase 1 because activation datoms use last-write-wins replay semantics. Rebuild cost scales with the number of distinct `(entity, attribute)` activation pairs, not with the cumulative count of activation writes, so the segmented log can grow indefinitely without degrading rebuild performance. Segment files can be backed up and archived externally at operator discretion.
- What happens when two segment files contain the same `tx` ULID? Expected: startup detects the collision, quarantines both conflicting segments to `~/.cortex/log.d/.quarantine/`, reports the collision via `cortex doctor`, and fails startup until the operator resolves the ambiguity. ULID collisions in the same nanosecond with identical randomness are cryptographically improbable but not impossible; the substrate must not silently accept them.
- What happens when a reflection run is interrupted mid-way after writing some but not all proposed frames? Expected: the reflection watermark advances per accepted frame (not per run), so the next invocation resumes from the first unwritten frame's source window. Scoped reflections (post-ingest, `--contradictions-only`) use a separate marker and do not disturb the global scheduled watermark.
- What happens when a cluster of concurrent `observe` writers exceeds the 50-writer contention scenario? Expected: at most 20% of concurrent writers time out under the 5-second per-segment lock (`SC-007` formalizes this bound). Remaining writers succeed or proceed with their own segment files once the contended segment frees or rolls.
- What happens when the Ollama-reported model digest at the start of a write differs from the digest reported at embedding time (e.g., operator ran `ollama pull` in a parallel shell)? Expected: Cortex caches the digest from a single `ollama show` call at the start of the write and verifies it against the embedding response; a mismatch aborts the write with exit `1` and a `MODEL_DIGEST_RACE` error. The operator re-runs after Ollama settles.
- What happens when a secret-detector match occurs in an LLM-generated trail summary or frame proposal? Expected: the generated content is discarded, the operation fails with `SECRET_DETECTED_IN_GENERATION`, no datoms are written for that step, and the matched rule name (not the matched string) is logged to `ops.log`.
- What happens when `cortex unevict <id>` runs against a non-evicted entry? Expected: the command returns exit `0` with a no-op status message and writes no datoms.

---

## Explicit Non-Behaviors

- The system must not expose `link` or `unlink` as authoring verbs because link creation is derived from the substrate.
- The system must not allow agents to write semantic or procedural frames directly because reflection is the only path into the semantic store.
- The system must not perform destructive deletion of datoms because retractions are part of the accretive model.
- The system must not require internet access for normal operation once Docker images and Ollama models are installed locally.
- The system must not silently upgrade pinned models during rebuild because replay determinism depends on using the originally recorded model metadata.
- The system must not treat embedding similarity alone as identity because PSI equality, not cosine similarity, governs subject identity.
- The system must not store raw source code as the primary artifact because Cortex stores understanding about code, not the code itself.
- The system must not interpolate user-provided observation bodies, facet values, or PSI identifiers directly into LLM prompts because prompt injection could corrupt link derivations, frame proposals, or community summaries.
- The system must not emit raw secret-bearing payloads to stderr, `~/.cortex/ops.log`, or any derived index. High-confidence secrets detected during observe or ingest MUST be redacted or rejected.
- The system must not bind managed services to non-loopback interfaces by default because local-operator scope does not require network exposure.

---

## Integration Boundaries

### Datom Log Filesystem

- **Data in**: Transaction-group datoms serialized as JSON Lines with checksum metadata.
- **Data out**: Replayed datoms (merge-sorted across segments), point-in-time scans, merge input, export output.
- **Contract**: Segmented log directory at `~/.cortex/log.d/` containing `<ulid>-<writer-id>.jsonl` segment files. Each segment is append-only under advisory `flock`, `O_APPEND`, one fsync per transaction group, and per-datom SHA-256 checksum. Segments are capped at `log.segment_max_size_mb` (default 64) and rolled transparently during writes. Merges drop external segment files in verbatim after checksum validation. Directory permission `0700`; segment file permission `0600`. Advisory locking assumes cooperative writers.
- **On failure**: Torn segment tails are truncated at startup; lock contention errors after 5 seconds; writes after the commit point are never rolled back. Non-cooperative writes are detectable via checksum validation but not preventable. Segment enumeration is resilient to individual corrupt segments (a corrupt segment is quarantined to `~/.cortex/log.d/.quarantine/` with an operational log entry; other segments continue to load).
- **Development**: Real filesystem — the segmented log is the source of truth, not a mock.

### Ollama

- **Data in**: Text for embeddings, concept extraction, trail summaries, link derivation, frame proposals, community summaries, and module summaries.
- **Data out**: Embeddings plus structured or free-text generations.
- **Contract**: Local HTTP service at `localhost:11434`; configured models include `nomic-embed-text` for embeddings and `llama3.1:8b-instruct` for generation; timeout budgets are operation-specific. Ollama MUST bind to localhost only. `cortex doctor` SHOULD verify Ollama is not listening on `0.0.0.0`. All LLM prompt construction uses structured templates with delimiters; user-provided content is never interpolated raw.
- **On failure**: Pre-commit operations fail with exit code `1`; committed writes are not rolled back; `doctor` reports missing models with pull guidance.
- **Development**: Real service — Phase 1 does not define a mock substitute.

### Weaviate

- **Data in**: Entries and frames with vectorizable text plus facets and metadata.
- **Data out**: Vector similarity results, nearest neighbors, and collection metadata.
- **Contract**: Local HTTP at the configured `endpoints.weaviate_http` (default `localhost:9397`) and gRPC at the configured `endpoints.weaviate_grpc` (default `localhost:50051`); derived index rebuildable from the datom log.
- **On failure**: Reads depending on Weaviate fail; writes may already be committed to the log and are repaired later by self-healing replay.
- **Development**: Real Docker-managed service.

### Neo4j with Graph Data Science

- **Data in**: Entries, frames, trails, communities, subjects, edges, and activation metadata.
- **Data out**: Cypher results, PPR scores, shortest paths, community detection results, and graph traversals.
- **Contract**: Bolt at the configured `endpoints.neo4j_bolt` (default `localhost:7687`). Auth credentials are generated on first `cortex up` and stored in `~/.cortex/config.yaml` (mode `0600`). `cortex doctor` verifies the credential is not the default bootstrap value. GDS plugin pre-baked into the custom image `cortex/neo4j-gds:<cortex-release-version>` built locally from `docker/neo4j-gds/Dockerfile` on first `cortex up` (see Infrastructure Topology). Required GDS procedures: `gds.pageRank.stream`, `gds.graph.project`, and either `gds.leiden.stream` (preferred) or `gds.louvain.stream` (fallback). Operator can force Louvain via `community_detection.algorithm: louvain`.
- **On failure**: Traversal, path, community, and default recall fail; committed writes remain in the log and are replayed later.
- **Development**: Real Docker-managed service built from source on each operator machine. Phase 1 is local-operator use only. Cortex distributes a `Dockerfile` that pulls GDS from Neo4j's official distribution at build time; it does not re-distribute Neo4j or GDS binaries. Commercial distribution is explicitly out of scope.

### Local Project Repositories

- **Data in**: Source files, `.gitignore`, directory structure, git metadata, and optionally custom ingest strategies.
- **Data out**: N/A.
- **Contract**: Filesystem reads plus standard git queries; ingest strategies are resolved by CLI flag, project config, language matrix, then fallback. Cortex MUST NOT follow symlinks outside the project root by default. Cortex MUST NOT ingest files from `~/.cortex/` or other Cortex internal directories.
- **On failure**: Missing path, unreadable repo, or malformed custom strategy returns a clear operational error.
- **Development**: Real local repositories.

### MemPalace Export

- **Data in**: Exported MemPalace drawers, diary items, entities, and related records.
- **Data out**: N/A.
- **Contract**: JSONL import file interpreted by the migration command. Validation: maximum record size, maximum total file size, JSON depth limits, and path canonicalization for the `--from-mempalace` argument.
- **On failure**: Malformed records are reported and skipped; successful records continue through the normal pipeline.
- **Development**: Real export artifact from the prior system.

### Docker Compose

- **Data in**: Compose file path, service definitions, environment variables, and local volume state.
- **Data out**: Container lifecycle state and logs.
- **Contract**: `cortex up`, `down`, and `status` wrap standard Docker Compose behavior for Weaviate and Neo4j only. Managed services bind to loopback only by default. `cortex up` waits for backend readiness (health endpoint OK) before returning success.
- **On failure**: Status reports degraded state; doctor reports the specific failing service and remediation.
- **Development**: Real Docker installation.

---

## Security

### Secret Handling

Cortex MUST NOT persist high-confidence secrets (API keys, tokens, passwords, private keys) detected in observation bodies, ingest content, or LLM outputs. The default behavior is to reject writes containing high-confidence secret patterns with a validation error. This applies to `observe`, `ingest`, summarization, reflection, analysis, and failure logging.

**Phase 1 detector**: Cortex ships with a built-in regex pattern set in `cortex/security/secrets/builtin.yaml` embedded in the binary. No external dependency; no network; no subprocess. The built-in ruleset covers:

- **Cloud credentials**: AWS access keys (`AKIA[0-9A-Z]{16}`), AWS secret keys, GCP service account JSON signatures, Azure connection strings.
- **Version-control tokens**: GitHub personal access tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`), GitHub App tokens, GitLab personal access tokens.
- **Messaging / collaboration**: Slack tokens (`xox[baprs]-...`), Discord bot tokens.
- **Private keys**: PEM-format headers for RSA, EC, OPENSSH, PGP, DSA.
- **JWTs**: Three-segment base64url with header-claim structure validation.
- **Generic high-entropy**: String literals adjacent to case-insensitive `password`, `secret`, `token`, or `api_key` identifiers, with Shannon entropy above a configurable threshold.

**Operator extension**: Additional patterns live in `~/.cortex/secrets.yaml` and are merged with the built-in set at startup. The file format is the same as `builtin.yaml`: named rules with a regex, an optional entropy floor, and a severity level. Operators can add project-specific patterns; they cannot disable built-in rules in Phase 1.

**Behavior on match**:
- `observe` or `ingest` rejects the write with exit code `2` and error code `SECRET_DETECTED`, reporting the matched rule name (not the matched string) in the error envelope.
- Reflection, analysis, and trail-summary LLM outputs are scanned before being written; a match causes the generated content to be discarded and the operation to fail with `SECRET_DETECTED_IN_GENERATION`.
- `ops.log` entries are scrubbed through the same ruleset before write; any matching substring is replaced with `[REDACTED:<rule-name>]`.
- No matched string is ever written to any backend, log file, or stderr. The detected rule name is logged; the secret itself is not.

`~/.cortex/ops.log` and stderr MUST NOT contain raw secret-bearing payloads under any code path.

### File Permissions

`~/.cortex/` and all files within MUST have owner-only permissions: `0700` for directories, `0600` for files. `cortex up` and `cortex doctor` verify these permissions.

### Service Binding

All managed services (Weaviate, Neo4j) bind to loopback (`127.0.0.1`) only in the default `docker-compose.yaml`. Ollama MUST bind to localhost only; `cortex doctor` SHOULD verify Ollama is not listening on `0.0.0.0`.

### Credential Management

Neo4j credentials are generated randomly on first `cortex up` and stored in `~/.cortex/config.yaml` (mode `0600`). `cortex doctor` warns if the bootstrap default credential is still in use.

### Input Sanitization

All LLM prompt construction uses structured templates with clear delimiters. User-provided content (observation bodies, facet values, PSI identifiers, project names) is treated as opaque data and never interpolated raw into prompts.

### Audit Trail

Administrative commands (`rebuild`, `merge`, `subject merge`, `pin`, `unpin`, `evict`, `migrate`) record the invoking agent or user identity (from `CORTEX_AGENT` or `USER` environment variable) in their datoms.

---

## BDD Scenarios

### Feature: Datom Log and Recovery

#### Background

- **Given** a Cortex installation with the configured local dependencies available

#### Scenario: Append and fsync a committed datom group

**Traces to**: User Story 1, Acceptance Scenario 1
**Category**: Happy Path

- **Given** an active trail
- **When** `cortex observe "Token validation race" --kind=ObservedRace --facets=domain:Security,artifact:Service,project:pay-gw` runs
- **Then** Cortex appends a transaction-group of datoms to the writer's current segment file under `~/.cortex/log.d/`
- **And** every datom in the group records a checksum
- **And** the group shares a single transaction ID

#### Scenario: Recover a torn log tail on startup

**Traces to**: User Story 1, Acceptance Scenario 2
**Category**: Edge Case

- **Given** a segment file under `~/.cortex/log.d/` ends with a truncated datom
- **When** any `cortex` command starts
- **Then** Cortex truncates that segment back to its last well-formed datom
- **And** appends a `log.recovered` audit datom describing the recovery

#### Scenario: Replay committed datoms into lagging backends

**Traces to**: User Story 1, Acceptance Scenario 3
**Category**: Alternate Path

- **Given** the log watermark is ahead of the Neo4j watermark
- **And** the Weaviate watermark matches the log
- **When** `cortex recall "retry logic"` starts
- **Then** Cortex replays the missing committed datoms into Neo4j before executing the query
- **And** updates the backend watermark afterward

#### Scenario: Rebuild uses pinned models and log replay

**Traces to**: User Story 1, Acceptance Scenario 4
**Category**: Happy Path

- **Given** a datom log with entries written using recorded embedding model metadata
- **When** `cortex rebuild` runs
- **Then** Cortex replays the log into fresh derived indexes
- **And** uses the pinned model metadata recorded on those entries
- **And** fails if the pinned model is unavailable

#### Scenario: Merge logs by transaction ID set union

**Traces to**: User Story 1, Acceptance Scenario 5
**Category**: Happy Path

- **Given** log A contains transactions `T1,T2,T3`
- **And** log B contains transactions `T2,T3,T4`
- **When** `cortex merge <log-b>` runs against log A
- **Then** the merged log contains `T1,T2,T3,T4`
- **And** no transaction appears twice

#### Scenario: Retraction preserves history while hiding the entity

**Traces to**: User Story 1, Acceptance Scenario 6 and 10
**Category**: Alternate Path

- **Given** an existing entry `entry:abc`
- **When** `cortex retract entry:abc --reason="superseded by ADR-12"` runs
- **Then** Cortex appends retraction datoms rather than mutating prior records
- **And** audit datoms record the invoking identity and the reason
- **And** default recall no longer surfaces `entry:abc`
- **And** `cortex history entry:abc` still returns the full lineage

#### Scenario: Cascade retraction covers derived children

**Traces to**: User Story 1, Acceptance Scenario 10
**Category**: Alternate Path

- **Given** frame `frame:retry-pattern-1` has three `DERIVED_FROM` exemplar entries
- **When** `cortex retract frame:retry-pattern-1 --cascade` runs
- **Then** Cortex retracts the frame and its `DERIVED_FROM` children
- **And** all retractions share audit metadata from the same invocation

#### Scenario: Lock timeout fails without partial writes

**Traces to**: User Story 1, Acceptance Scenario 7
**Category**: Error Path

- **Given** another writer holds the datom-log lock for more than 5 seconds
- **When** a second writer runs `cortex observe`
- **Then** the second writer exits with operational code `1`
- **And** no partial datoms from the second writer appear in the log

#### Scenario: As-of query excludes later facts

**Traces to**: User Story 1, Acceptance Scenario 8
**Category**: Happy Path

- **Given** entry A is written at `T1`
- **And** entry B is written at `T2` where `T2 > T1`
- **When** `cortex as-of T1 recall "query matching both"` runs
- **Then** only entry A is visible to the query


#### Scenario: Self-healing tail replay on startup

**Traces to**: User Story 1, Acceptance Scenario 3
**Category**: Error Path

- **Given** a datom log with transaction T5 committed but Neo4j `:CortexMeta` watermark at T3
- **When** any cortex command is run
- **Then** transactions T4 and T5 are replayed to Neo4j and Weaviate before the command proceeds
- **And** the `:CortexMeta` watermark is updated to T5

#### Scenario: Rebuild fails on model digest drift without --accept-drift

**Traces to**: User Story 1, Acceptance Scenario 9
**Category**: Error Path

- **Given** a datom log with entries recorded under `embedding_model_digest` `sha256:aaa`
- **And** the currently installed model has digest `sha256:bbb`
- **When** `cortex rebuild` runs without `--accept-drift`
- **Then** Cortex fails with a pinned-model-drift error listing affected entries
- **And** does not modify any derived state

#### Scenario: Rebuild with --accept-drift re-embeds and audits

**Traces to**: User Story 1, Acceptance Scenario 9
**Category**: Alternate Path

- **Given** a datom log with entries recorded under a different model digest
- **When** `cortex rebuild --accept-drift` runs
- **Then** Cortex re-embeds affected entries under the current model
- **And** records `model_rebind` audit datoms for each re-embedded entry

---

### Feature: Episodic Observation Capture

#### Scenario: Write an observation with required facets

**Traces to**: User Story 2, Acceptance Scenario 1
**Category**: Happy Path

- **Given** an active trail
- **When** `cortex observe "API has TOCTOU race" --kind=ObservedRace --facets=domain:Security,artifact:Service,project:payment-gw` runs
- **Then** Cortex creates an episodic entry with the required facets
- **And** attaches it to the active trail
- **And** indexes it in both backends

#### Scenario Outline: Reject invalid observe input with standard validation errors

**Traces to**: User Story 2, Acceptance Scenario 2
**Category**: Error Path

- **Given** an active trail
- **When** `cortex observe "<body>" <args> --json` runs
- **Then** the command exits with code `2`
- **And** the error envelope includes `<error_code>`
- **And** no datom is written

**Examples**:

| body | args | error_code |
|------|------|------------|
| valid text | `--facets=domain:Security,project:foo` | `MISSING_KIND` |
| valid text | `--kind=InvalidKind --facets=domain:Security,project:foo` | `UNKNOWN_KIND` |
|  | `--kind=Observation --facets=domain:Security,project:foo` | `EMPTY_BODY` |

#### Scenario: Resolve and attach a PSI subject during observe

**Traces to**: User Story 2, Acceptance Scenario 3
**Category**: Happy Path

- **Given** no existing subject for `lib/go/redis`
- **When** `cortex observe "Redis connection pooling issue" --kind=Observation --facets=domain:Reliability,project:cache-svc --subject=lib/go/redis` runs
- **Then** Cortex creates the canonical subject
- **And** writes an `ABOUT` relationship from the entry to that subject

#### Scenario: Persist accepted A-MEM links

**Traces to**: User Story 2, Acceptance Scenario 4
**Category**: Happy Path

- **Given** the graph already contains several semantically similar entries
- **When** a new observation is written
- **Then** Cortex queries the top 5 nearest neighbors from Weaviate
- **And** persists only the links whose confidence and edge-specific thresholds pass

#### Scenario: Skip derived-link writes when structured LLM output is malformed

**Traces to**: User Story 2, Acceptance Scenario 5
**Category**: Edge Case

- **Given** the observe command input is otherwise valid
- **And** the link-derivation LLM returns malformed JSON
- **When** the observe pipeline runs
- **Then** the entry datoms are still committed
- **But** no derived-link datoms are appended for that step

---

### Feature: Trail Lifecycle

#### Scenario: Begin a trail and receive a usable trail ID

**Traces to**: User Story 3, Acceptance Scenario 1
**Category**: Happy Path

- **Given** no active trail
- **When** `cortex trail begin --agent=grill-spec --name="auth review"` runs
- **Then** Cortex returns a new trail ID
- **And** the trail can be reused in subsequent `cortex observe` calls

#### Scenario: End a trail and persist its narrative summary

**Traces to**: User Story 3, Acceptance Scenario 2
**Category**: Happy Path

- **Given** an active trail containing three ordered observations
- **When** `cortex trail end` runs
- **Then** Cortex records `ended_at`
- **And** persists an LLM-generated summary for the trail
- **And** preserves the observation order in `IN_TRAIL.order`

#### Scenario: Show a completed trail with entries and derived outcomes

**Traces to**: User Story 3, Acceptance Scenario 3
**Category**: Happy Path

- **Given** a completed trail with derived frames
- **When** `cortex trail show <trail-id>` runs
- **Then** the output includes trail metadata, summary, ordered entries, and outcome frames

#### Scenario: List trails with filters and pagination

**Traces to**: User Story 3, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** many trails across several agents and projects
- **When** `cortex trail list --agent=alpha --project=foo --limit=20 --offset=20` runs
- **Then** only trails matching both filters are returned
- **And** the second page is returned in stable order

---

### Feature: Retrieval and Recall

#### Scenario: Default recall returns ranked results with context

**Traces to**: User Story 4, Acceptance Scenario 1
**Category**: Happy Path

- **Given** entries and frames about retry logic exist across multiple projects
- **When** `cortex recall "retry logic" --json` runs in default mode
- **Then** Cortex extracts concepts from the query
- **And** runs PPR from the resolved seed set
- **And** reranks candidates by ACT-R activation
- **And** returns trail context, community context, and "why surfaced" traces

#### Scenario: Default recall writes reinforcement datoms

**Traces to**: User Story 4, Acceptance Scenario 2
**Category**: Happy Path

- **Given** entry `entry:x` has previously been retrieved twice
- **When** `entry:x` is returned by default recall
- **Then** Cortex appends datoms updating `base_activation`, `retrieval_count`, and `last_retrieved_at`

#### Scenario: Similar mode bypasses graph ranking

**Traces to**: User Story 4, Acceptance Scenario 3
**Category**: Alternate Path

- **Given** a populated Cortex corpus
- **When** `cortex recall "error handling" --mode=similar` runs
- **Then** Cortex uses pure vector similarity from Weaviate
- **And** does not run the default PPR plus ACT-R ranking path

#### Scenario: Traverse mode walks outward from a seed entry

**Traces to**: User Story 4, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** an entry `entry:a` connected to several neighbors within depth 2
- **When** `cortex recall "any" --mode=traverse --from=entry:a --depth=2` runs
- **Then** Cortex returns the reachable neighborhood from `entry:a`
- **And** includes the traversed typed edges in the response

#### Scenario: Path mode returns the shortest graph path

**Traces to**: User Story 4, Acceptance Scenario 5
**Category**: Alternate Path

- **Given** entries `entry:a` and `entry:b` are connected through the graph
- **When** `cortex path entry:a entry:b` runs
- **Then** Cortex returns the shortest path between the two entries

#### Scenario: Community mode returns a community summary and members

**Traces to**: User Story 4, Acceptance Scenario 6
**Category**: Alternate Path

- **Given** community `community:123` exists at level 2
- **When** `cortex community show community:123 --level=2` runs
- **Then** Cortex returns the community summary and top members for that level

#### Scenario: Surprise mode resurfaces stale knowledge

**Traces to**: User Story 4, Acceptance Scenario 7
**Category**: Alternate Path

- **Given** several relevant entries are below the default visibility threshold
- **When** `cortex recall "retry logic" --mode=surprise` runs
- **Then** Cortex may return stale but relevant entries that default mode would exclude

#### Scenario: Retrieval pagination is deterministic

**Traces to**: User Story 4, Acceptance Scenario 8
**Category**: Edge Case

- **Given** more than 100 results match a recall query
- **When** `cortex recall "timeout" --limit=10 --offset=20 --json` runs
- **Then** Cortex returns a deterministic 10-result page
- **And** within a single invocation, the sort order is stable and offset/limit produce consistent pages. Cross-invocation determinism requires that no intervening writes or decay updates change the sort keys.

#### Scenario: Empty recall returns success with zero results

**Traces to**: User Story 4, Acceptance Scenario 1
**Category**: Edge Case

- **Given** Cortex contains no matching entries or frames
- **When** `cortex recall "nonexistent concept"` runs
- **Then** Cortex returns an empty result set with exit code `0`

---

### Feature: Code Ingestion

#### Scenario: Ingest a project using the configured language strategy

**Traces to**: User Story 5, Acceptance Scenario 1
**Category**: Happy Path

- **Given** a Go project with non-test source files in one package directory
- **When** `cortex ingest ~/projects/foo --project=foo` runs
- **Then** Cortex groups those files as one module using the Go `per-package` strategy
- **And** writes summary and pattern observations through the standard pipeline

#### Scenario: Incremental re-ingest retracts deleted-file entries

**Traces to**: User Story 5, Acceptance Scenario 2
**Category**: Happy Path

- **Given** project `foo` was previously ingested at commit `abc123`
- **And** one file has changed and one file has been deleted
- **When** `cortex ingest ~/projects/foo --project=foo` runs again
- **Then** only the changed module is reprocessed
- **And** entries derived from the deleted file receive retraction datoms

#### Scenario: Successful ingest creates a synthesized trail and scoped reflection

**Traces to**: User Story 5, Acceptance Scenario 3
**Category**: Happy Path

- **Given** ingest is enabled with default settings
- **When** a project ingest finishes successfully
- **Then** Cortex writes a synthesized ingest trail for that run
- **And** triggers scoped reflection over the ingest window

#### Scenario: Cross-project analysis remains opt-in after ingest

**Traces to**: User Story 5, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** `post_ingest_analyze` is false
- **When** an ingest finishes without `--analyze`
- **Then** Cortex does not run cross-project analysis automatically

#### Scenario: Resume completes only missing modules

**Traces to**: User Story 5, Acceptance Scenario 5
**Category**: Error Path

- **Given** an ingest trail shows 10 of 20 modules completed
- **When** `cortex ingest resume --project=foo` runs
- **Then** the remaining 10 modules are processed
- **And** the first 10 are not duplicated

#### Scenario: Ingest status reports the last successful run

**Traces to**: User Story 5, Acceptance Scenario 6
**Category**: Happy Path

- **Given** project `foo` has already been ingested
- **When** `cortex ingest status --project=foo` runs
- **Then** the output reports the last ingested SHA, timestamp, counts, and resume state

---

### Feature: Reflection and Consolidation

#### Scenario: Reflection defaults to the last successful watermark

**Traces to**: User Story 6, Acceptance Scenario 1
**Category**: Happy Path

- **Given** a previous reflection run recorded a success watermark
- **When** `cortex reflect` runs with no `--since`
- **Then** Cortex selects episodic candidates newer than that watermark

#### Scenario: Reflection accepts a qualifying cluster

**Traces to**: User Story 6, Acceptance Scenario 2
**Category**: Happy Path

- **Given** 5 episodic entries about the same bug pattern recorded across at least 2 distinct sessions
- **When** `cortex reflect` runs
- **Then** the cluster passes the size, timestamp, cosine, and MDL thresholds
- **And** a frame proposal is eligible for writing

#### Scenario: Reflection writes a typed frame with provenance

**Traces to**: User Story 6, Acceptance Scenario 3
**Category**: Happy Path

- **Given** a qualifying cluster and a valid frame proposal
- **When** the proposal is accepted
- **Then** Cortex writes the frame with slot claims, `DERIVED_FROM` edges, and the frame schema version

#### Scenario: Reflection creates a supersession edge for a more general frame

**Traces to**: User Story 6, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** an existing frame already covers project A
- **When** reflection writes a more general frame covering projects A and B
- **Then** Cortex emits a `SUPERSEDES` edge from the new frame to the old

#### Scenario: High-importance contradictions trigger scoped re-reflection

**Traces to**: User Story 6, Acceptance Scenario 5
**Category**: Alternate Path

- **Given** a new entry contradicts a `cross_project=true` frame
- **When** the contradiction is detected during write-time linking
- **Then** Cortex writes the contradiction edge
- **And** triggers scoped re-reflection for that contradicted cluster

#### Scenario: Dry-run explain shows rejected frame candidates

**Traces to**: User Story 6, Acceptance Scenario 6
**Category**: Error Path

- **Given** some candidate clusters fail the acceptance thresholds
- **When** `cortex reflect --dry-run --explain` runs
- **Then** Cortex reports the failed criteria for each rejected candidate
- **But** writes no frame datoms

#### Scenario: Direct frame writes are rejected

**Traces to**: User Story 6, Acceptance Scenario 7
**Category**: Error Path

- **Given** an agent attempts `cortex observe "..." --kind=BugPattern --facets=domain:Security,project:test`
- **When** the command is validated
- **Then** Cortex rejects the write because reflection is the only semantic-store entry point

#### Scenario: Reflection refreshes only changed communities

**Traces to**: User Story 6, Acceptance Scenario 8
**Category**: Happy Path

- **Given** reflection writes frames affecting one existing community
- **When** the run completes
- **Then** Cortex regenerates summaries only for new or changed communities
- **And** leaves unrelated community summaries untouched

#### Scenario: Contradiction-only reflection processes open contradictions

**Traces to**: User Story 6, Acceptance Scenario 5
**Category**: Alternate Path

- **Given** several frames have open `CONTRADICTS` edges
- **When** `cortex reflect --contradictions-only` runs
- **Then** Cortex re-reflects only the clusters containing contradicted frames
- **And** may emit `SUPERSEDES` edges where appropriate

---

### Feature: Cross-Project Analysis

#### Scenario: Cross-project analysis accepts only multi-project clusters

**Traces to**: User Story 7, Acceptance Scenario 1
**Category**: Happy Path

- **Given** two projects share a retry pattern and a third project does not
- **When** `cortex analyze --find-patterns` runs
- **Then** accepted clusters contain exemplars from at least two projects
- **And** no single project contributes more than 70 percent of any accepted cluster

#### Scenario: Cross-project frames receive an importance boost

**Traces to**: User Story 7, Acceptance Scenario 2
**Category**: Happy Path

- **Given** a cross-project frame and a similar single-project frame both match a query
- **When** default recall runs
- **Then** the cross-project frame ranks higher because of the configured importance boost

#### Scenario: Project-scoped analysis excludes non-selected projects

**Traces to**: User Story 7, Acceptance Scenario 3
**Category**: Alternate Path

- **Given** projects alpha, beta, and gamma all contain a shared pattern
- **When** `cortex analyze --find-patterns --projects=alpha,beta` runs
- **Then** only alpha and beta exemplars appear in accepted frames

#### Scenario: Dry-run explain reports proposed cross-project frames

**Traces to**: User Story 7, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** cross-project signal exists
- **When** `cortex analyze --find-patterns --dry-run --explain` runs
- **Then** Cortex lists considered clusters, rejected candidates, and proposed frames
- **But** writes no datoms

#### Scenario: Cross-project analysis performs a full community refresh

**Traces to**: User Story 7, Acceptance Scenario 5
**Category**: Happy Path

- **Given** cross-project analysis accepted at least one new frame
- **When** the analysis run completes
- **Then** Cortex performs a full community detection re-run over the semantic graph
- **And** refreshes summaries for changed communities

---

### Feature: Subject Governance and Communities

#### Scenario: PSI validation enforces required namespaces

**Traces to**: User Story 8, Acceptance Scenario 1
**Category**: Happy Path

- **Given** a subject PSI candidate `lib/go/redis`
- **When** Cortex validates it
- **Then** the PSI is accepted because it matches a required namespace pattern

#### Scenario: Subject merge writes canonical and alias facts

**Traces to**: User Story 8, Acceptance Scenario 2
**Category**: Alternate Path

- **Given** two PSIs refer to the same subject
- **When** `cortex subject merge <psi-a> <psi-b>` runs
- **Then** Cortex records one PSI as canonical
- **And** records the other as an alias
- **But** deletes neither subject from the log

#### Scenario: Communities expose hierarchical summaries

**Traces to**: User Story 8, Acceptance Scenario 3
**Category**: Happy Path

- **Given** the semantic graph has enough structure for community detection
- **When** communities are refreshed
- **Then** Cortex produces a three-level hierarchy with summaries at each level
- **And** `cortex communities --level=2` lists only communities at level 2

#### Scenario: Manual link commands remain unavailable

**Traces to**: User Story 8, Acceptance Scenario 4
**Category**: Error Path

- **Given** a user is at the CLI
- **When** they run `cortex link entry:a entry:b`
- **Then** Cortex rejects the command because manual link authoring is unsupported

---

### Feature: Forgetting and Paging Controls

#### Scenario: New writes seed initial activation

**Traces to**: User Story 9, Acceptance Scenario 1
**Category**: Happy Path

- **Given** a brand new entry is being written
- **When** the write commits
- **Then** Cortex records `base_activation=1.0`
- **And** leaves `retrieval_count=0` until recall actually surfaces the entry

#### Scenario: Decay hides an unpinned entry below the threshold

**Traces to**: User Story 9, Acceptance Scenario 2
**Category**: Happy Path

- **Given** an unpinned entry has not been retrieved for a long interval
- **When** ACT-R decay is applied
- **Then** its activation falls below `0.05`
- **And** default recall excludes it

#### Scenario: Surprise retrieval can revive a stale entry

**Traces to**: User Story 9, Acceptance Scenario 3
**Category**: Alternate Path

- **Given** an entry is below the default visibility threshold
- **When** `cortex recall "query" --mode=surprise` returns that entry
- **Then** reinforcement raises its activation
- **And** a later default recall may include it again

#### Scenario: Pin and evict change visibility without deleting datoms

**Traces to**: User Story 9, Acceptance Scenario 4
**Category**: Alternate Path

- **Given** entries `entry:pinned` and `entry:evicted`
- **When** `cortex pin entry:pinned` and `cortex evict entry:evicted` run
- **Then** the pinned entry resists normal decay
- **And** the evicted entry drops from default recall without being retracted

#### Scenario: Evict entry forces low activation and blocks reinforcement

**Traces to**: User Story 9, Acceptance Scenario 5
**Category**: Alternate Path

- **Given** entry Y with `base_activation=0.8`
- **When** `cortex evict entry:Y` is run
- **Then** entry Y's activation is forced to `0.0`
- **And** a sticky `evicted_at` attribute is written
- **And** entry Y drops from default retrieval because `0.0 < 0.05` (inclusive visibility threshold)
- **And** subsequent `cortex recall --mode=surprise`, `--mode=traverse`, and `--mode=similar` do NOT surface entry Y even if it matches
- **But** entry Y remains in the datom log and is accessible via `cortex history` and explicit `cortex get entry:Y`

#### Scenario: Unevict restores reinforcement eligibility

**Traces to**: User Story 9, Acceptance Scenario 6
**Category**: Alternate Path

- **Given** entry Y was previously evicted
- **When** `cortex unevict entry:Y` runs
- **Then** Cortex writes `evicted_at_retracted`
- **And** reinforcement is re-enabled
- **And** a subsequent recall that surfaces entry Y raises its activation normally

---

### Feature: Infrastructure and Operational Health

#### Scenario: Start managed infrastructure

**Traces to**: User Story 10, Acceptance Scenario 1
**Category**: Happy Path

- **Given** Docker is installed and running
- **When** `cortex up` runs
- **Then** Weaviate and Neo4j start from the configured Compose file

#### Scenario: Build produces a macOS ARM64 binary

**Traces to**: User Story 10, Acceptance Scenario 6
**Category**: Happy Path

- **Given** the Cortex source tree is ready to build
- **When** the release build pipeline runs
- **Then** it emits a single Go binary targeting `darwin/arm64`

#### Scenario: Status reports service state

**Traces to**: User Story 10, Acceptance Scenario 2
**Category**: Happy Path

- **Given** Weaviate and Neo4j are running, Ollama is running
- **When** `cortex status` is run
- **Then** the output shows each service as "running" or "stopped"
- **And** the datom log size and entry count are reported

#### Scenario: Status reports running and degraded services

**Traces to**: User Story 10, Acceptance Scenario 2
**Category**: Alternate Path

- **Given** Weaviate is healthy and Neo4j is stopped
- **When** `cortex status --json` runs
- **Then** the output marks Weaviate as running
- **And** marks Neo4j as stopped or degraded

#### Scenario: Doctor reports missing dependency details

**Traces to**: User Story 10, Acceptance Scenario 3
**Category**: Error Path

- **Given** Neo4j is running without the GDS plugin
- **And** the configured reflection model is not available in Ollama
- **When** `cortex doctor --json` runs
- **Then** the report contains separate failures for GDS absence and missing Ollama model
- **And** includes remediation guidance for both

#### Scenario: Shut down managed infrastructure cleanly

**Traces to**: User Story 10, Acceptance Scenario 4
**Category**: Happy Path

- **Given** the managed services are running
- **When** `cortex down` runs
- **Then** the managed containers stop cleanly

#### Scenario: Doctor verifies Ollama model availability

**Traces to**: User Story 10, Acceptance Scenario 3
**Category**: Error Path

- **Given** Ollama is running but `nomic-embed-text` is not pulled
- **When** `cortex doctor` is run
- **Then** the report includes a failure: "Model nomic-embed-text: NOT FOUND"
- **And** remediation: "Run: ollama pull nomic-embed-text"

#### Scenario: Offline operation succeeds with local dependencies

**Traces to**: User Story 10, Acceptance Scenario 5
**Category**: Edge Case

- **Given** the required containers and Ollama models are already present locally
- **And** the host has no internet connectivity
- **When** observe, recall, reflect, ingest, and analyze commands run
- **Then** they succeed without external network access

#### Scenario: Doctor detects Leiden unavailability and warns about Louvain fallback

**Traces to**: User Story 10, Acceptance Scenario 3
**Category**: Alternate Path

- **Given** Neo4j is running with GDS but `gds.leiden.stream` is unavailable
- **And** `gds.louvain.stream` is available
- **When** `cortex doctor` runs
- **Then** it warns that Leiden is unavailable and community detection will use Louvain
- **But** does not report a hard failure for community detection

#### Scenario: First-time setup generates Neo4j credentials

**Traces to**: User Story 10, Acceptance Scenario 7
**Category**: Happy Path

- **Given** no previous Cortex configuration exists
- **When** `cortex up` runs for the first time
- **Then** Cortex generates a random Neo4j password
- **And** stores it in `~/.cortex/config.yaml` with mode `0600`

#### Scenario: Benchmark validates performance envelopes

**Traces to**: User Story 10, Acceptance Scenario 8
**Category**: Happy Path

- **Given** a healthy Cortex stack on P1-dev hardware
- **When** `cortex bench --corpus=small --profile=P1-dev` runs
- **Then** it populates the small fixture corpus
- **And** runs the scripted operation sequence
- **And** reports p50/p95/p99 per operation type
- **And** exits `0` because all envelopes pass

#### Scenario: Doctor verifies file permissions

**Traces to**: User Story 10, Acceptance Scenario 3
**Category**: Error Path

- **Given** `~/.cortex/` has world-readable permissions
- **When** `cortex doctor` runs
- **Then** it reports a security failure for incorrect file permissions
- **And** includes remediation: "Run: chmod 0700 ~/.cortex/"

---

### Feature: MemPalace Migration

#### Scenario: Migrate MemPalace records into Cortex entities

**Traces to**: User Story 11, Acceptance Scenario 1
**Category**: Happy Path

- **Given** a MemPalace export containing drawers, diary items, and entities
- **When** `cortex migrate --from-mempalace=<path>` runs
- **Then** drawers become episodic entries
- **And** diary items become `SessionReflection` entries on synthesized trails
- **And** entities become Cortex PSI subjects

#### Scenario: Migration output flows through the normal write pipeline

**Traces to**: User Story 11, Acceptance Scenario 2
**Category**: Happy Path

- **Given** native Cortex entries already reference `lib/go/redis`
- **And** the MemPalace export also contains Redis-related items
- **When** migration runs
- **Then** migrated entries reuse or merge into the same canonical subject
- **And** the migration report includes created, reused, and skipped counts

---

## Test-Driven Development Plan

### Test Hierarchy

| Level | Scope | Purpose |
|-------|-------|---------|
| Unit | Datom serialization, checksuming, locking rules, ACT-R math, threshold evaluation, frame-schema validation, PSI validation, pagination helpers | Validates deterministic logic in isolation |
| Integration | Observe, recall, reflect, analyze, ingest, migration, subject, trail, doctor, status, and rebuild workflows against real local services or faithful test doubles | Validates components and persistence boundaries work together |
| E2E | Full two-project ingestion through recall plus analyze, offline execution, concurrent writers | Validates the Phase 1 value proposition and operating constraints from the user perspective |

### Test Implementation Order

Write these tests BEFORE implementing the feature code. Order: unit first, then integration, then E2E. Within each level, order by dependency.

| Order | Test Name | Level | Traces to BDD Scenario | Description |
|-------|-----------|-------|------------------------|-------------|
| 1 | `test_datom_checksum_roundtrip` | Unit | Scenario: Append and fsync a committed datom group | Verifies serialized datoms include stable checksums and round-trip correctly |
| 2 | `test_log_tail_recovery_truncates_invalid_suffix` | Unit | Scenario: Recover a torn log tail on startup | Verifies torn tails are truncated to the last valid datom |
| 3 | `test_log_lock_timeout_returns_operational_error` | Unit | Scenario: Lock timeout fails without partial writes | Verifies the 5-second lock timeout produces no writes |
| 4 | `test_error_envelope_validation_shape` | Unit | Scenario Outline: Reject invalid observe input with standard validation errors | Verifies JSON error envelope format |
| 5 | `test_ulid_generation_is_monotonic_per_writer` | Unit | Scenario: Append and fsync a committed datom group | Verifies transaction IDs are monotonic per writer |
| 6 | `test_actr_decay_equation` | Unit | Scenario: Decay hides an unpinned entry below the threshold | Verifies decay math with the configured exponent |
| 7 | `test_initial_activation_seed_on_write` | Unit | Scenario: New writes seed initial activation | Verifies new entries start with `base_activation=1.0` and `retrieval_count=0` |
| 8 | `test_activation_reinforcement` | Unit | Scenario: Default recall writes reinforcement datoms | Verifies reinforcement updates after recall |
| 9 | `test_reflection_threshold_evaluation` | Unit | Scenario: Reflection accepts a qualifying cluster | Verifies size, timestamp, cosine, and MDL thresholds |
| 10 | `test_frame_schema_validation` | Unit | Scenario: Direct frame writes are rejected | Verifies built-in and custom frame schema validation |
| 11 | `test_psi_namespace_validation` | Unit | Scenario: PSI validation enforces required namespaces | Verifies accepted and rejected PSI forms |
| 12 | `test_pagination_default_resolution` | Unit | Scenario: Retrieval pagination is deterministic | Verifies limit and offset defaults for human and JSON modes |
| 13 | `test_observe_write_pipeline` | Integration | Scenario: Write an observation with required facets | Verifies observe writes to the log and both backends |
| 14 | `test_observe_subject_resolution` | Integration | Scenario: Resolve and attach a PSI subject during observe | Verifies canonical subject creation and ABOUT edge creation |
| 15 | `test_observe_amem_link_thresholds` | Integration | Scenario: Persist accepted A-MEM links | Verifies top-K neighbor lookup and thresholded link persistence |
| 16 | `test_observe_malformed_link_output_skips_links` | Integration | Scenario: Skip derived-link writes when structured LLM output is malformed | Verifies entry success without derived-link datoms |
| 17 | `test_startup_self_heal_replays_lagging_backend` | Integration | Scenario: Replay committed datoms into lagging backends | Verifies watermark comparison and replay |
| 18 | `test_rebuild_uses_pinned_model_metadata` | Integration | Scenario: Rebuild uses pinned models and log replay | Verifies rebuild honors recorded model metadata |
| 19 | `test_merge_logs_dedup_by_tx` | Integration | Scenario: Merge logs by transaction ID set union | Verifies merge deduplication behavior |
| 20 | `test_retraction_and_history` | Integration | Scenario: Retraction preserves history while hiding the entity | Verifies hidden-from-recall plus visible-in-history behavior |
| 21 | `test_as_of_filters_future_datoms` | Integration | Scenario: As-of query excludes later facts | Verifies point-in-time visibility |
| 22 | `test_trail_begin_end_summary` | Integration | Scenario: End a trail and persist its narrative summary | Verifies trail lifecycle and summary persistence |
| 23 | `test_trail_list_filters_and_paginates` | Integration | Scenario: List trails with filters and pagination | Verifies filtered paging |
| 24 | `test_recall_default_pipeline` | Integration | Scenario: Default recall returns ranked results with context | Verifies concept extraction, PPR, rerank, and context assembly |
| 25 | `test_recall_reinforcement_datoms` | Integration | Scenario: Default recall writes reinforcement datoms | Verifies persisted reinforcement after default recall |
| 26 | `test_recall_mode_similar` | Integration | Scenario: Similar mode bypasses graph ranking | Verifies pure vector mode |
| 27 | `test_recall_mode_traverse` | Integration | Scenario: Traverse mode walks outward from a seed entry | Verifies bounded traversal mode |
| 28 | `test_recall_mode_path` | Integration | Scenario: Path mode returns the shortest graph path | Verifies shortest-path retrieval |
| 29 | `test_recall_mode_community` | Integration | Scenario: Community mode returns a community summary and members | Verifies community retrieval mode |
| 30 | `test_recall_mode_surprise` | Integration | Scenario: Surprise mode resurfaces stale knowledge | Verifies stale-item resurfacing and follow-up reinforcement |
| 31 | `test_recall_empty_result` | Integration | Scenario: Empty recall returns success with zero results | Verifies zero-result recall returns success without reinforcement writes |
| 32 | `test_recall_pagination_stability` | Integration | Scenario: Retrieval pagination is deterministic | Verifies page stability across repeated calls |
| 33 | `test_reflection_watermark_default_window` | Integration | Scenario: Reflection defaults to the last successful watermark | Verifies default `--since` behavior |
| 34 | `test_reflection_frame_creation` | Integration | Scenario: Reflection writes a typed frame with provenance | Verifies accepted frames and provenance edges |
| 35 | `test_reflection_supersedes_existing_frame` | Integration | Scenario: Reflection creates a supersession edge for a more general frame | Verifies supersession behavior |
| 36 | `test_reflection_dry_run_explain` | Integration | Scenario: Dry-run explain shows rejected frame candidates | Verifies rejection observability without writes |
| 37 | `test_reflection_contradiction_scope` | Integration | Scenario: High-importance contradictions trigger scoped re-reflection | Verifies immediate scoped re-reflection rules |
| 38 | `test_community_refresh_changed_only` | Integration | Scenario: Reflection refreshes only changed communities | Verifies incremental summary refresh |
| 39 | `test_ingest_full_project` | Integration | Scenario: Ingest a project using the configured language strategy | Verifies module grouping and writes for a real fixture project |
| 40 | `test_ingest_incremental_and_deleted_files` | Integration | Scenario: Incremental re-ingest retracts deleted-file entries | Verifies changed-module and deleted-file handling |
| 41 | `test_ingest_trail_and_post_reflect` | Integration | Scenario: Successful ingest creates a synthesized trail and scoped reflection | Verifies ingest trail plus scoped reflection |
| 42 | `test_ingest_analyze_is_opt_in` | Integration | Scenario: Cross-project analysis remains opt-in after ingest | Verifies post-ingest analyze defaults |
| 43 | `test_ingest_resume_is_idempotent` | Integration | Scenario: Resume completes only missing modules | Verifies safe resume |
| 44 | `test_ingest_status_reports_last_run` | Integration | Scenario: Ingest status reports the last successful run | Verifies ingest status output |
| 45 | `test_cross_project_analysis_acceptance_rules` | Integration | Scenario: Cross-project analysis accepts only multi-project clusters | Verifies project-count and share thresholds |
| 46 | `test_cross_project_projects_filter` | Integration | Scenario: Project-scoped analysis excludes non-selected projects | Verifies `--projects` scoping |
| 47 | `test_cross_project_importance_boost` | Integration | Scenario: Cross-project frames receive an importance boost | Verifies retrieval weighting effect |
| 48 | `test_subject_merge_alias_persistence` | Integration | Scenario: Subject merge writes canonical and alias facts | Verifies accretive subject merge behavior |
| 49 | `test_no_manual_link_verbs` | Integration | Scenario: Manual link commands remain unavailable | Verifies missing `link` and `unlink` behavior |
| 50 | `test_initial_activation_and_decay` | Integration | Scenario: New writes seed initial activation | Verifies write-time activation plus later decay |
| 51 | `test_pin_and_evict_controls` | Integration | Scenario: Pin and evict change visibility without deleting datoms | Verifies pin and evict semantics |
| 52 | `test_up_starts_managed_infrastructure` | Integration | Scenario: Start managed infrastructure | Verifies `cortex up` starts the managed stack |
| 53 | `test_build_targets_darwin_arm64` | Integration | Scenario: Build produces a macOS ARM64 binary | Verifies the produced binary targets `darwin/arm64` |
| 54 | `test_status_reports_service_state` | Integration | Scenario: Status reports running and degraded services | Verifies `cortex status` output |
| 55 | `test_doctor_reports_dependency_failures` | Integration | Scenario: Doctor reports missing dependency details | Verifies doctor checks and remediation output |
| 56 | `test_down_stops_managed_infrastructure` | Integration | Scenario: Shut down managed infrastructure cleanly | Verifies `cortex down` stops the managed stack |
| 57 | `test_migration_reuses_existing_subjects` | Integration | Scenario: Migration output flows through the normal write pipeline | Verifies MemPalace migration subject unification and reporting |
| 58 | `test_e2e_cross_project_value` | E2E | Scenario: Cross-project analysis accepts only multi-project clusters | Ingest two real fixture projects, recall shared knowledge, analyze, and verify the cross-project value criterion |
| 59 | `test_e2e_offline_operation` | E2E | Scenario: Offline operation succeeds with local dependencies | Verifies end-to-end offline behavior |
| 60 | `test_e2e_concurrent_writers` | E2E | Scenario: Lock timeout fails without partial writes | Verifies concurrent observe operations keep the log consistent under contention |
| 61 | `test_ops_log_structured_format` | Unit | (ops.log structured logging) | Verifies ops.log entries are valid JSONL with required fields |
| 62 | `test_prompt_template_sanitization` | Unit | (input sanitization) | Verifies user content is delimited, not interpolated raw |
| 63 | `test_rebuild_digest_drift_fails` | Integration | Scenario: Rebuild fails on model digest drift | Verifies rebuild refuses when digests differ |
| 64 | `test_rebuild_accept_drift_rebinds` | Integration | Scenario: Rebuild with --accept-drift | Verifies re-embedding and model_rebind datoms |
| 65 | `test_secret_redaction_observe` | Integration | (secret handling) | Verifies secrets are rejected/redacted in observe |
| 66 | `test_secret_redaction_ingest` | Integration | (secret handling) | Verifies secrets are rejected/redacted in ingest |
| 67 | `test_neo4j_credential_generation` | Integration | Scenario: First-time setup generates Neo4j credentials | Verifies random password generation and storage |
| 68 | `test_ingest_symlink_outside_root_skipped` | Integration | (symlink safety) | Verifies symlinks outside project root are skipped |
| 69 | `test_bench_small_corpus_p1_dev` | E2E | Scenario: Benchmark validates performance envelopes | Verifies cortex bench exit code on P1-dev |
| 70 | `test_reflect_contradictions_only` | Integration | Scenario: Contradiction-only reflection | Verifies scoped re-reflection of contradicted frames |
| 71 | `test_file_permissions_check` | Integration | Scenario: Doctor verifies file permissions | Verifies doctor detects incorrect permissions |
| 72 | `test_leiden_louvain_fallback` | Integration | Scenario: Doctor detects Leiden unavailability | Verifies automatic Louvain fallback |
| 73 | `test_graceful_shutdown_transaction_integrity` | Integration | (graceful shutdown) | Verifies SIGINT leaves consistent state |
| 74 | `test_admin_command_audit_identity` | Integration | (audit trail) | Verifies admin commands record agent identity, classified by operator-scope vs agent-scope paging |
| 75 | `test_segmented_log_merge_sort_on_read` | Unit | (segmented log) | Verifies the reader merge-sorts datoms across multiple segment files by `tx` ULID |
| 76 | `test_segment_roll_on_size_cap` | Integration | (segmented log) | Verifies segment rolls cleanly at `log.segment_max_size_mb` and transaction groups never span segments |
| 77 | `test_segment_quarantine_on_checksum_failure` | Integration | Scenario: Recover a torn log tail on startup | Verifies a corrupt segment is quarantined, other segments continue to load, and `doctor` reports it |
| 78 | `test_segment_tx_collision_fails_startup` | Integration | (startup validation) | Verifies a ULID collision between segments fails startup loudly instead of silently accepting |
| 79 | `test_merge_drops_external_segments_in_place` | Integration | Scenario: Merge logs by transaction ID set union | Verifies `cortex merge` drops foreign segment files into `log.d/` and the reader picks them up |
| 80 | `test_activation_lww_replay` | Unit | (activation replay) | Verifies last-write-wins replay: only the highest-`tx` activation per `(entity, attribute)` is applied to derived indexes |
| 81 | `test_retract_entry_hides_but_preserves_history` | Integration | Scenario: Retraction preserves history while hiding the entity | Verifies `cortex retract` appends retraction datoms, hides from default recall, preserves history |
| 82 | `test_retract_cascade_retracts_derived` | Integration | (retract cascade) | Verifies `cortex retract <frame-id> --cascade` retracts `DERIVED_FROM` children |
| 83 | `test_evict_sticky_blocks_reinforcement` | Integration | Scenario: Pin and evict change visibility without deleting datoms | Verifies evict sticks across subsequent recalls in all modes; reinforcement cannot restore |
| 84 | `test_unevict_reenables_reinforcement` | Integration | (unevict) | Verifies `cortex unevict` retracts the sticky flag and reinforcement works again |
| 85 | `test_reflection_watermark_per_frame_advance` | Integration | Scenario: Reflection defaults to the last successful watermark | Verifies per-frame advancement: an interrupted reflection that wrote 3 of 5 frames resumes from the 4th |
| 86 | `test_scoped_reflection_uses_separate_marker` | Integration | (reflection scope markers) | Verifies post-ingest and `--contradictions-only` use separate markers from the scheduled watermark |
| 87 | `test_migrated_excluded_from_cross_project` | Integration | (migration) | Verifies migrated entries are excluded from `cortex analyze --find-patterns` by default |
| 88 | `test_migrated_included_with_flag` | Integration | (migration override) | Verifies `--include-migrated` includes migrated content in cross-project analysis |
| 89 | `test_secret_detected_in_generation` | Integration | (secret handling in LLM output) | Verifies a secret detected in an LLM-generated trail summary fails with `SECRET_DETECTED_IN_GENERATION` |
| 90 | `test_model_digest_race_aborts_write` | Integration | (digest race) | Verifies a cached-vs-embedding digest mismatch aborts the write with `MODEL_DIGEST_RACE` |
| 91 | `test_doctor_quick_under_5_seconds` | Integration | (doctor time budget) | Verifies `cortex doctor --quick` completes in under 5 seconds |
| 92 | `test_doctor_parallel_full_checks` | Integration | (doctor parallelism) | Verifies `cortex doctor --full` runs independent checks in parallel |
| 93 | `test_community_detection_on_tiny_corpus` | Integration | (tiny corpus edge case) | Verifies community detection on a sub-threshold corpus returns empty with exit `0` |
| 94 | `test_concurrent_writers_timeout_bound` | E2E | (concurrent writers) | Verifies at most 20% of 50 concurrent writers time out under contention |
| 95 | `test_frame_registry_enforces_reflection_only` | Unit | Scenario: Direct frame writes are rejected | Verifies the eight reflection-only frame types reject direct `observe` writes and the three agent-writable types succeed |
| 96 | `test_custom_neo4j_gds_image_built_locally` | Integration | (Custom Neo4j+GDS Image) | Verifies `cortex up` builds `cortex/neo4j-gds:<version>` from `docker/neo4j-gds/Dockerfile` on first run if absent and reuses the cached image on subsequent runs |
| 97 | `test_up_readiness_contract` | Integration | Scenario: Start managed infrastructure | Verifies the full readiness contract: Docker check, ordered start, Weaviate `/v1/.well-known/ready`, Neo4j Bolt ping + `gds.version()`, Ollama `/api/tags` model presence, 90-second total budget |
| 98 | `test_up_partial_success_fails` | Integration | (readiness contract all-or-nothing) | Verifies `cortex up` exits non-zero when one backend reaches readiness but another does not within the budget |
| 99 | `test_compose_resource_limits_match_spec` | Integration | (resource allocation) | Verifies the generated `docker-compose.yaml` declares the request and limit values from the Resource Allocation table |
| 100 | `test_volumes_survive_down_and_purge_flag` | Integration | (volume topology) | Verifies `cortex_weaviate_data` and `cortex_neo4j_data` survive `cortex down` and are removed only by `cortex down --purge` with confirmation |
| 101 | `test_upgrade_path_export_down_up_rebuild` | E2E | (upgrade posture) | Verifies the supported upgrade workflow: export the log, `down --purge`, install new release, `up`, `rebuild` — and asserts the derived indexes match the pre-upgrade state to the layered-determinism guarantees |
| 102 | `test_doctor_host_prerequisites` | Integration | Scenario: Doctor reports missing dependency details | Verifies `cortex doctor` checks the full Host Prerequisites table and refuses `cortex up` on hard-fail prerequisites |

### Test Datasets

#### Dataset: Observation Validation and Error Envelopes

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | `cortex observe "x" --kind=Observation --facets=domain:Security,project:foo` | Valid minimal | Entry created | BDD Scenario: Write an observation with required facets | Minimum valid episodic write |
| 2 | `cortex observe "x" --facets=domain:Security,project:foo --json` | Missing required | Error envelope with `MISSING_KIND`, exit `2` | BDD Scenario Outline: Reject invalid observe input with standard validation errors | No datom written |
| 3 | `cortex observe "x" --kind=InvalidKind --facets=domain:Security,project:foo --json` | Invalid enum | Error envelope with `UNKNOWN_KIND`, exit `2` | BDD Scenario Outline: Reject invalid observe input with standard validation errors | Unknown kind |
| 4 | `cortex observe "" --kind=Observation --facets=domain:Security,project:foo --json` | Empty | Error envelope with `EMPTY_BODY`, exit `2` | BDD Scenario Outline: Reject invalid observe input with standard validation errors | Empty body |
| 5 | `cortex observe "redis issue" --kind=Observation --facets=domain:Reliability,project:cache --subject=lib/go/redis` | Valid subject | Entry plus `ABOUT` edge | BDD Scenario: Resolve and attach a PSI subject during observe | Canonical subject attach |

#### Dataset: Datom Log Durability and Recovery

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | Well-formed transaction-group with checksums | Happy path | Appended and replayable | BDD Scenario: Append and fsync a committed datom group | Normal committed write |
| 2 | Log tail truncated mid-record | Torn write | Tail truncated, `log.recovered` appended | BDD Scenario: Recover a torn log tail on startup | Crash after partial write |
| 3 | Lock held for 6 seconds by another writer | Timeout | Exit `1`, no datoms written | BDD Scenario: Lock timeout fails without partial writes | Contention exceeds budget |
| 4 | Lagging Neo4j watermark, current Weaviate watermark | Drift | Missing datoms replayed into Neo4j | BDD Scenario: Replay committed datoms into lagging backends | Self-healing startup |
| 5 | Merge log A `[T1,T2,T3]` and log B `[T2,T3,T4]` | Overlap | `[T1,T2,T3,T4]` | BDD Scenario: Merge logs by transaction ID set union | Dedup by `tx` |

#### Dataset: Retrieval Modes and Pagination

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | Default recall, no `--limit` | Default | Top 10 results | BDD Scenario: Default recall returns ranked results with context | Uses retrieval default limit |
| 2 | `--mode=similar` | Alternate mode | Pure vector results | BDD Scenario: Similar mode bypasses graph ranking | No PPR rerank |
| 3 | `--mode=traverse --depth=2` | Alternate mode | Bounded neighborhood from seed | BDD Scenario: Traverse mode walks outward from a seed entry | Requires `--from` |
| 4 | `cortex path <id-a> <id-b>` | Alternate mode | Shortest path | BDD Scenario: Path mode returns the shortest graph path | Graph-only path |
| 5 | `--mode=community --community=<id> --level=2` | Alternate mode | Community summary plus members | BDD Scenario: Community mode returns a community summary and members | Topic-centric read |
| 6 | `--mode=surprise` on stale corpus | Edge | Stale entries surfaced | BDD Scenario: Surprise mode resurfaces stale knowledge | Serendipity path |
| 7 | `--limit=10 --offset=20` | Pagination | Deterministic third page | BDD Scenario: Retrieval pagination is deterministic | Stable ordering required |

#### Dataset: Reflection and Cross-Project Thresholds

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | 5 exemplars, 2 timestamps, cosine `0.72`, MDL `1.40` | Above threshold | Frame accepted | BDD Scenario: Reflection accepts a qualifying cluster | Scheduled reflection pass |
| 2 | 3 exemplars, 1 timestamp, cosine `0.81`, MDL `1.60` | Below timestamp floor | Candidate rejected | BDD Scenario: Dry-run explain shows rejected frame candidates | Fails distinct timestamp rule |
| 3 | 3 exemplars, 2 timestamps, cosine `0.60`, MDL `1.50` | Below cosine floor | Candidate rejected | BDD Scenario: Dry-run explain shows rejected frame candidates | Fails coherence |
| 4 | 4 exemplars from 2 projects, max project share `0.50`, MDL `1.18` | Cross-project valid | Frame accepted with `cross_project=true` | BDD Scenario: Cross-project analysis accepts only multi-project clusters | Meets relaxed threshold |
| 5 | 4 exemplars from 2 projects, max project share `0.75`, MDL `1.25` | Cross-project invalid | Candidate rejected | BDD Scenario: Cross-project analysis accepts only multi-project clusters | Fails share threshold |
| 6 | Contradiction against `cross_project=true` frame | Priority exception | Scoped re-reflection triggered | BDD Scenario: High-importance contradictions trigger scoped re-reflection | Immediate contradiction handling |

#### Dataset: Activation, Pinning, and Eviction

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | New entry at write time | Initial state | `base_activation=1.0`, `retrieval_count=0` | BDD Scenario: New writes seed initial activation | Encoding event only |
| 2 | Unpinned entry, 90 days without retrieval | Stale | Activation below `0.05`, hidden from default recall | BDD Scenario: Decay hides an unpinned entry below the threshold | Long idle interval |
| 3 | Surprise recall returns stale entry | Reinforcement | Activation rises above threshold | BDD Scenario: Surprise retrieval can revive a stale entry | Re-entry to default recall |
| 4 | `cortex pin <id>` | Pinned | No decay while pinned | BDD Scenario: Pin and evict change visibility without deleting datoms | Paging protection |
| 5 | `cortex evict <id>` | Explicit eviction | Forced to `0.0` activation, no retraction | BDD Scenario: Evict entry forces low activation | Visibility control only |

#### Dataset: Ingestion File and Strategy Handling

| # | Input | Boundary Type | Expected Output | Traces to | Notes |
|---|-------|---------------|-----------------|-----------|-------|
| 1 | Go package with 3 `.go` files | Language-specific | One module summary entry | BDD Scenario: Ingest a project using the configured language strategy | Go `per-package` |
| 2 | Java file with one public class | Language-specific | One class-level module entry | BDD Scenario: Ingest a project using the configured language strategy | Java `per-class` |
| 3 | Python file | Language-specific | One file-level module entry | BDD Scenario: Ingest a project using the configured language strategy | Python `per-file` |
| 4 | 300 KB generated source file | Above max | Warning and skip | BDD Scenario: Ingest a project using the configured language strategy | Exceeds 262144-byte limit |
| 5 | File deleted between ingests | Incremental | Retraction datoms for old derived entries | BDD Scenario: Incremental re-ingest retracts deleted-file entries | History retained |
| 6 | Partial ingest with 10 of 20 modules complete | Resume | Remaining 10 modules processed only | BDD Scenario: Resume completes only missing modules | Idempotent resume |

### Regression Test Requirements

**If new functionality:**

> No regression impact from an existing Cortex implementation is assumed for Phase 1. Integration seams protected by: service-health tests (`test_status_reports_service_state`, `test_doctor_reports_dependency_failures`), substrate integrity tests (`test_observe_write_pipeline`, `test_startup_self_heal_replays_lagging_backend`, `test_rebuild_uses_pinned_model_metadata`), and end-to-end value tests (`test_e2e_cross_project_value`, `test_e2e_offline_operation`).

---

## Functional Requirements

- **FR-001**: System MUST store all persisted knowledge as immutable datoms in an append-only **segmented** JSONL log at `~/.cortex/log.d/` that is the sole source of truth. Readers MUST enumerate all segment files and merge-sort datoms by `tx` ULID to produce a single causal stream.
- **FR-002**: System MUST protect per-segment log appends with advisory exclusive locking (`flock(LOCK_EX)` per segment), `O_APPEND`, one fsync per transaction group, checksum-per-datom validation, and a 5-second lock timeout. The lock is per-segment, not global.
- **FR-003**: System MUST validate and repair torn log tails on every command startup and record recovery as audit datoms.
- **FR-004**: System MUST self-heal derived-index drift on every command startup by comparing log and backend watermarks and replaying missing committed datoms.
- **FR-005**: System MUST rebuild derived indexes entirely from the datom log using the `embedding_model_name` and `embedding_model_digest` recorded on each entry, MUST fail loudly with a pinned-model-drift error when the installed model digest differs from stored digests unless `--accept-drift` is passed, and MUST record `model_rebind` audit datoms when re-embedding under a different digest.
- **FR-006**: System MUST support merging datom logs by dropping external segment files into `~/.cortex/log.d/` after checksum validation. Merge is trivial set union by `tx` ULID at read time; no rewrite of existing segments occurs.
- **FR-007**: System MUST model deletion as retraction datoms and MUST support `history` and `as-of` queries over the log. The operator-facing verb is `cortex retract <id> [--cascade] [--reason=<text>]`, which takes any Cortex entity identifier (entry, frame, trail, subject, community), writes retraction datoms for the target (and dependent entities under `--cascade`), records invoking identity and reason as audit datoms, hides the retracted entity from default retrieval, and leaves full lineage queryable via `cortex history` and `cortex as-of`.
- **FR-008**: System MUST require `kind`, `domain`, and `project` facets for every episodic observation.
- **FR-009**: System MUST reject invalid observe input with standard JSON error envelopes and exit code `2`.
- **FR-010**: System MUST support PSI subject resolution during observe writes.
- **FR-011**: System MUST perform write-time A-MEM linking using top-5 nearest neighbors and the configured confidence thresholds.
- **FR-012**: System MUST capture every session as a trail with ordered entries and an LLM-generated trail summary.
- **FR-013**: System MUST implement the default recall pipeline as concept extraction plus seed resolution plus PPR plus ACT-R rerank.
- **FR-014**: System MUST support alternate retrieval modes `similar`, `traverse`, `path`, `community`, and `surprise`.
- **FR-015**: System MUST reinforce returned results by appending activation-update datoms (`base_activation`, `retrieval_count`, `last_retrieved_at`) after recall. Activation attributes use **last-write-wins replay semantics**: during rebuild or self-healing, only the activation datom with the highest `tx` per `(entity, attribute)` pair is applied to derived indexes. Older activation datoms remain in the log (accessible via `cortex history`) but are no-ops for backend state. This keeps the log pure append-only while bounding rebuild cost by the number of distinct entities, not the cumulative count of retrievals.
- **FR-016**: System MUST support `--limit` and `--offset` pagination for list-like query commands and provide deterministic ordering for paged results.
- **FR-017**: System MUST restrict semantic and procedural frame creation to the reflection pipeline.
- **FR-018**: System MUST qualify reflection candidates using the configured size, timestamp, cosine, and MDL thresholds.
- **FR-019**: System MUST default reflection candidate selection to the last successful reflection watermark when `--since` is omitted. The scheduled-reflection watermark MUST advance **per accepted frame** (not per run) so that an interrupted reflection resumes correctly without reprocessing already-written frames. Scoped reflections (post-ingest, `--project=`, `--contradictions-only`) MUST use separate scope-specific markers and MUST NOT advance the global scheduled-reflection watermark.
- **FR-020**: System MUST support `cortex reflect --dry-run --explain` and surface rejection reasons for non-accepted candidates.
- **FR-021**: System MUST validate built-in and user-defined frame schemas and record the frame schema version on asserted frame claims.
- **FR-022**: System MUST support language-aware code ingestion with the configured strategy matrix, includes, excludes, and `module_size_limit_bytes`.
- **FR-023**: System MUST support incremental re-ingestion using the recorded last-ingested SHA and MUST emit retraction datoms for deleted-file-derived entries.
- **FR-024**: System MUST synthesize ingest trails and MUST run scoped post-ingest reflection by default.
- **FR-025**: System MUST keep post-ingest cross-project analysis opt-in by default while supporting `--analyze` and configuration override.
- **FR-026**: System MUST perform cross-project analysis with at least two projects, a maximum 70 percent single-project share, and the relaxed MDL threshold.
- **FR-027**: System MUST mark accepted cross-project frames with `cross_project=true` and boost their retrieval importance.
- **FR-028**: System MUST perform a full semantic-graph community detection refresh (Leiden preferred, Louvain fallback) after cross-project analysis.
- **FR-029**: System MUST govern PSI subjects with required namespace prefixes, immutable canonical IDs, additive aliases, and explicit accretive merge behavior.
- **FR-030**: System MUST maintain a default 3-level hierarchical community structure with summaries and query commands for listing and showing communities.
- **FR-031**: System MUST seed new entries with initial activation and MUST include entries in default retrieval only when `base_activation >= visibility_threshold`.
- **FR-032**: System MUST support explicit paging controls `pin`, `unpin`, `evict`, and `unevict` without deleting datoms. `evict` sets a sticky `evicted_at` attribute that forces `base_activation` to `0.0` and blocks reinforcement across all retrieval modes. `unevict` retracts the sticky flag via `evicted_at_retracted` and re-enables reinforcement. `pin` on an evicted entry implicitly unevicts it and additionally marks it pinned.
- **FR-033**: System MUST provide infrastructure commands `up`, `down`, `status`, and `doctor`.
- **FR-034**: System MUST have `doctor` validate Docker, Weaviate reachability, Neo4j auth and credential strength, GDS procedures with Leiden-preferred/Louvain-fallback detection, configured Ollama models with digest verification, frame schemas, ingest concurrency, log-segment recoverability and quarantined-segment count, pinned-model digest availability, `~/.cortex/` file permissions, disk space, service endpoint binding, and the secret-detector ruleset. `cortex doctor --quick` runs only checks whose individual execution time is bounded under 500 ms and returns in under 5 seconds total. `cortex doctor --full` (default) runs all checks, parallelized with `doctor.parallelism` (default `4`).
- **FR-035**: System MUST operate offline once required local dependencies are installed and healthy.
- **FR-036**: System MUST provide `--json` output mode for all commands and standardize CLI exit codes as `0` success, `1` operational failure, and `2` validation failure.
- **FR-037**: System MUST preserve log integrity under concurrent reads and writes, with post-commit backend failures repaired via self-healing replay.
- **FR-038**: System MUST migrate MemPalace exports into Cortex entries, trails, and subjects while producing a migration report. Every migrated entity MUST carry a `migrated=true` facet attribute recording the source system and migration timestamp. Migrated entries MUST be excluded from `cortex analyze --find-patterns` cross-project candidate selection by default; the operator can opt in via `--include-migrated`.
- **FR-039**: System MUST build as a single Go binary targeting macOS ARM64.
- **FR-040**: System MUST meet the Phase 1 performance envelopes verified by `cortex bench` on the authoritative P1-dev profile: small corpus recall under 2.0 s at p95 and observe under 3.0 s at p95; medium corpus recall under 3.5 s at p95 and observe under 5.0 s at p95.
- **FR-041**: System MUST NOT expose `link` or `unlink` authoring verbs.
- **FR-042**: System MUST NOT allow direct semantic or procedural frame creation through `observe`.
- **FR-043**: System MUST structure `~/.cortex/ops.log` as JSON Lines with timestamp, level, invocation_id (ULID), component, tx, entity_ids, message, and error fields, and MUST rotate at the configured size limit.
- **FR-044**: System MUST complete or discard the current transaction group on SIGINT/SIGTERM and leave watermarks consistent.
- **FR-045**: System MUST reject or redact high-confidence secrets detected in observation bodies, ingest content, and LLM-generated content (trail summaries, frame proposals, community summaries), using the built-in regex pattern set in `cortex/security/secrets/builtin.yaml` merged with any operator patterns at `~/.cortex/secrets.yaml`. Matches cause writes to fail with `SECRET_DETECTED` (exit `2`) or `SECRET_DETECTED_IN_GENERATION` (exit `1`). The matched rule name is logged; the matched string MUST NOT appear in any log file, stderr, or derived index. `ops.log` entries are scrubbed through the same ruleset before write, with matches replaced by `[REDACTED:<rule-name>]`.
- **FR-046**: System MUST provide `cortex bench` as a bundled benchmark command that validates performance envelopes.
- **FR-047**: System MUST hold the advisory `flock` only for the log append and `fsync()` commit, executing all backend writes, LLM calls, and watermark updates outside the lock.
- **FR-048**: System MUST use structured prompt templates with clear delimiters for all LLM calls, treating user-provided content as opaque data.
- **FR-049**: System MUST NOT follow symlinks outside the project root during ingestion, MUST NOT ingest from `~/.cortex/`, and MUST support a configurable path deny-list.
- **FR-050**: System MUST generate Neo4j credentials on first setup and store them in mode `0600` configuration, and MUST NOT ship with hardcoded default credentials.
- **FR-051**: System MUST record `embedding_model_name` and `embedding_model_digest` on every Entry and Frame at write time. The digest MUST be obtained from Ollama via a single `ollama show` call at the start of the write and cached for the duration of the invocation; a mismatch between the cached digest and the digest reported on the embedding call MUST abort the write with `MODEL_DIGEST_RACE` (exit `1`) and write no datoms. Fallback to name-only is a degraded mode surfaced by `cortex doctor`.
- **FR-052**: System MUST support `cortex reflect --contradictions-only` for scoped re-reflection of frames with open contradictions.
- **FR-053**: System MUST bind managed services to loopback only by default and enforce owner-only file permissions on `~/.cortex/`.
- **FR-054**: System MUST record the invoking agent or user identity in datoms for administrative commands. Administrative commands are classified as **operator-scope** (`rebuild`, `merge`, `subject merge`, `migrate`, `retract`, `project forget`) and **agent-scope paging** (`pin`, `unpin`, `evict`, `unevict`). Both scopes record identity; the classification exists to support future permission models without restructuring the audit trail. Phase 1 does not enforce different permissions between the two scopes.
- **FR-055**: System MUST enumerate and load all segment files under `~/.cortex/log.d/` at startup, validate each segment's tail, merge-sort datoms by `tx` ULID, and quarantine any segment failing checksum validation to `~/.cortex/log.d/.quarantine/` with an operational log entry. Startup MUST NOT fail because of a single corrupt segment unless a `tx` ULID collision is detected between two segments, in which case startup MUST fail loudly and report via `cortex doctor`.
- **FR-056**: System MUST cap each log segment at `log.segment_max_size_mb` (default 64 MB) and transparently roll to a new segment (`<ulid>-<writer-id>.jsonl`) when the current segment exceeds the cap. Segment rolling is atomic with respect to transaction groups — a transaction group MUST NOT span two segments.

---

## Success Criteria

- **SC-001**: A committed datom write survives process crashes, startup tail validation repairs any torn suffix, and self-healing replay restores lagging backends without operator intervention.
- **SC-002**: `cortex rebuild` under the pinned model digest produces Layer 1 byte-identical log, Layer 2 structurally identical graph, Layer 3 cosine >= 0.98 embeddings. Rebuild under a different digest fails loudly unless `--accept-drift` is passed.
- **SC-003**: `cortex recall "<cross-cutting concept>"` over two ingested fixture projects returns results from both projects in its top 10 results. This success criterion is **automated** and runs in CI against fixture repositories.
- **SC-004**: `cortex analyze --find-patterns` over at least two ingested fixture projects produces at least one `cross_project=true` frame whose exemplars span those projects. This success criterion is **automated** and runs in CI.
- **SC-004a** (operator-gated, **load-bearing**, not CI): Phase 1 is not considered complete until the operator has successfully ingested **two of their own real projects** (not fixtures) and validated that `cortex recall "<cross-cutting concept>"` returns observations from both projects, and that `cortex analyze --find-patterns` produces at least one `cross_project=true` frame whose `DERIVED_FROM` edges span those projects. Fixture-based tests (SC-003, SC-004) validate the code path; only this operator-gated criterion validates that Cortex delivers on real-world data. The operator records the validating commit, the recall query, and the resulting cross-project frame ID in `~/.cortex/acceptance.md` as a durable record.
- **SC-005**: `cortex doctor --json` reports every failed dependency or configuration check as a distinct machine-readable error and passes when the local stack is healthy. `cortex doctor --quick` completes in under 5 seconds regardless of backend state.
- **SC-006**: On P1-dev hardware with the small corpus, `cortex bench --profile=P1-dev --corpus=small` exits `0`. On the medium corpus, `cortex bench --profile=P1-dev --corpus=medium` exits `0`.
- **SC-007**: Under a 50-concurrent-writer `cortex observe` contention test, no segment file is corrupted, no committed write is lost, every successful writer's datoms appear in the merged log, and **no more than 20%** of concurrent writers fail with `LOCK_TIMEOUT`. Writers failing with lock timeout write no partial datoms. This bound is enforced by `test_e2e_concurrent_writers`.
- **SC-008**: Ingesting a real fixture project produces module entries, PSI subjects, a synthesized ingest trail, and a scoped reflection pass without internet access.
- **SC-009**: The released binary builds and runs on macOS ARM64 as a single Go executable.
- **SC-010**: `cortex doctor --json` detects and warns about Leiden unavailability with Louvain fallback, verifies installed model digests, and checks file permissions.
- **SC-011**: No raw secrets appear in `~/.cortex/ops.log`, stderr output, or derived indexes after ingesting a repository containing known secret patterns.
- **SC-012**: `cortex bench --profile=P1-ci --corpus=small` exits `0` with the 2x CI multiplier applied.
- **SC-013**: Administrative commands record invoking identity in their datoms.
- **SC-014**: The segmented log correctly loads under mixed conditions: multiple segment files, a quarantined corrupt segment, a cross-machine merge that added foreign segments, and activation-datom LWW replay across segments all produce a consistent derived index.
- **SC-015**: `cortex retract` writes retraction datoms for any entity type, optionally cascades to `DERIVED_FROM` children, hides retracted entities from default retrieval, records invoking identity and reason, and preserves full lineage in `cortex history`.
- **SC-016**: `cortex evict` is sticky across all retrieval modes until explicitly reversed by `cortex unevict` or `cortex pin`; evicted entries do not reappear via reinforcement.
- **SC-017**: Secret-pattern matches in observation bodies, ingest content, and LLM-generated outputs (trail summaries, frame proposals, community summaries) cause the responsible operation to fail with the appropriate `SECRET_DETECTED*` error and write no datoms containing the matched substring.
- **SC-018**: On a machine meeting Host Prerequisites, `cortex up` from a cold start (no prior containers, no prior Neo4j image) builds the custom `cortex/neo4j-gds:<version>` image, starts Weaviate and Neo4j, passes the full readiness contract including `gds.version()` and Ollama model presence, and returns within the 90-second total budget.
- **SC-019**: Two operators who build Phase 1 from the same Cortex release on independent machines meeting Host Prerequisites reach byte-identical derived state (Layer 1), structurally identical graph state (Layer 2), and embedding cosine ≥ 0.98 (Layer 3) after ingesting the same fixture corpus, provided they pinned the same Ollama model digest at install time. This is the infrastructure-level reproducibility guarantee that SC-002 depends on.
- **SC-020**: `cortex down --purge` removes managed-service volumes after operator confirmation without touching `~/.cortex/log.d/`, and a subsequent `cortex up && cortex rebuild` restores derived state from the log alone.

---

## Traceability Matrix

| Requirement | User Story | BDD Scenario(s) | Test Name(s) |
|-------------|-----------|-----------------|--------------|
| FR-001 | US-1 | Append and fsync a committed datom group | `test_datom_checksum_roundtrip`, `test_observe_write_pipeline` |
| FR-002 | US-1 | Append and fsync a committed datom group, Lock timeout fails without partial writes | `test_datom_checksum_roundtrip`, `test_log_lock_timeout_returns_operational_error`, `test_e2e_concurrent_writers` |
| FR-003 | US-1 | Recover a torn log tail on startup | `test_log_tail_recovery_truncates_invalid_suffix` |
| FR-004 | US-1 | Replay committed datoms into lagging backends, Self-healing tail replay on startup | `test_startup_self_heal_replays_lagging_backend` |
| FR-005 | US-1 | Rebuild uses pinned models and log replay, Rebuild fails on model digest drift without --accept-drift, Rebuild with --accept-drift re-embeds and audits | `test_rebuild_uses_pinned_model_metadata`, `test_rebuild_digest_drift_fails`, `test_rebuild_accept_drift_rebinds` |
| FR-006 | US-1 | Merge logs by transaction ID set union | `test_merge_logs_dedup_by_tx` |
| FR-007 | US-1 | Retraction preserves history while hiding the entity, As-of query excludes later facts | `test_retraction_and_history`, `test_as_of_filters_future_datoms` |
| FR-008 | US-2 | Write an observation with required facets | `test_observe_write_pipeline` |
| FR-009 | US-2 | Scenario Outline: Reject invalid observe input with standard validation errors | `test_error_envelope_validation_shape` |
| FR-010 | US-2 | Resolve and attach a PSI subject during observe | `test_observe_subject_resolution` |
| FR-011 | US-2 | Persist accepted A-MEM links, Skip derived-link writes when structured LLM output is malformed | `test_observe_amem_link_thresholds`, `test_observe_malformed_link_output_skips_links` |
| FR-012 | US-3 | Begin a trail and receive a usable trail ID, End a trail and persist its narrative summary | `test_trail_begin_end_summary` |
| FR-013 | US-4 | Default recall returns ranked results with context, Empty recall returns success with zero results | `test_recall_default_pipeline`, `test_recall_empty_result` |
| FR-014 | US-4 | Similar mode bypasses graph ranking, Traverse mode walks outward from a seed entry, Path mode returns the shortest graph path, Community mode returns a community summary and members, Surprise mode resurfaces stale knowledge | `test_recall_mode_similar`, `test_recall_mode_traverse`, `test_recall_mode_path`, `test_recall_mode_community`, `test_recall_mode_surprise` |
| FR-015 | US-4 | Default recall writes reinforcement datoms | `test_activation_reinforcement`, `test_recall_reinforcement_datoms` |
| FR-016 | US-4, US-3 | Retrieval pagination is deterministic, List trails with filters and pagination | `test_pagination_default_resolution`, `test_recall_pagination_stability`, `test_trail_list_filters_and_paginates` |
| FR-017 | US-6 | Direct frame writes are rejected | `test_frame_schema_validation` |
| FR-018 | US-6 | Reflection accepts a qualifying cluster | `test_reflection_threshold_evaluation`, `test_reflection_frame_creation` |
| FR-019 | US-6 | Reflection defaults to the last successful watermark | `test_reflection_watermark_default_window` |
| FR-020 | US-6 | Dry-run explain shows rejected frame candidates | `test_reflection_dry_run_explain` |
| FR-021 | US-6 | Reflection writes a typed frame with provenance | `test_frame_schema_validation`, `test_reflection_frame_creation` |
| FR-022 | US-5 | Ingest a project using the configured language strategy | `test_ingest_full_project` |
| FR-023 | US-5 | Incremental re-ingest retracts deleted-file entries | `test_ingest_incremental_and_deleted_files` |
| FR-024 | US-5 | Successful ingest creates a synthesized trail and scoped reflection | `test_ingest_trail_and_post_reflect` |
| FR-025 | US-5 | Cross-project analysis remains opt-in after ingest | `test_ingest_analyze_is_opt_in` |
| FR-026 | US-7 | Cross-project analysis accepts only multi-project clusters | `test_cross_project_analysis_acceptance_rules`, `test_e2e_cross_project_value` |
| FR-027 | US-7 | Cross-project frames receive an importance boost | `test_cross_project_importance_boost` |
| FR-028 | US-7 | Cross-project analysis performs a full community refresh | `test_community_refresh_changed_only`, `test_cross_project_analysis_acceptance_rules` |
| FR-029 | US-8 | PSI validation enforces required namespaces, Subject merge writes canonical and alias facts | `test_psi_namespace_validation`, `test_subject_merge_alias_persistence` |
| FR-030 | US-8 | Communities expose hierarchical summaries | `test_community_refresh_changed_only`, `test_recall_mode_community` |
| FR-031 | US-9 | New writes seed initial activation, Decay hides an unpinned entry below the threshold | `test_initial_activation_seed_on_write`, `test_actr_decay_equation`, `test_initial_activation_and_decay` |
| FR-032 | US-9 | Pin and evict change visibility without deleting datoms, Evict entry forces low activation | `test_pin_and_evict_controls` |
| FR-033 | US-10 | Start managed infrastructure, Status reports running and degraded services, Doctor reports missing dependency details, Shut down managed infrastructure cleanly | `test_up_starts_managed_infrastructure`, `test_status_reports_service_state`, `test_doctor_reports_dependency_failures`, `test_down_stops_managed_infrastructure` |
| FR-034 | US-10 | Doctor reports missing dependency details, Doctor verifies Ollama model availability, Doctor detects Leiden unavailability and warns about Louvain fallback, Doctor verifies file permissions | `test_doctor_reports_dependency_failures`, `test_file_permissions_check`, `test_leiden_louvain_fallback` |
| FR-035 | US-10 | Offline operation succeeds with local dependencies | `test_e2e_offline_operation` |
| FR-036 | US-2, US-10 | Scenario Outline: Reject invalid observe input with standard validation errors, Status reports running and degraded services, Doctor reports missing dependency details | `test_error_envelope_validation_shape`, `test_status_reports_service_state`, `test_doctor_reports_dependency_failures` |
| FR-037 | US-1, US-10 | Lock timeout fails without partial writes, Replay committed datoms into lagging backends | `test_log_lock_timeout_returns_operational_error`, `test_startup_self_heal_replays_lagging_backend`, `test_e2e_concurrent_writers` |
| FR-038 | US-11 | Migrate MemPalace records into Cortex entities, Migration output flows through the normal write pipeline | `test_migration_reuses_existing_subjects` |
| FR-039 | US-10 | Build produces a macOS ARM64 binary | `test_build_targets_darwin_arm64` |
| FR-040 | US-4, US-2 | Benchmark validates performance envelopes | `test_bench_small_corpus_p1_dev` |
| FR-041 | US-8 | Manual link commands remain unavailable | `test_no_manual_link_verbs` |
| FR-042 | US-6 | Direct frame writes are rejected | `test_frame_schema_validation` |
| FR-043 | US-10 | (ops.log structured logging) | `test_ops_log_structured_format` |
| FR-044 | US-10 | (graceful shutdown) | `test_graceful_shutdown_transaction_integrity` |
| FR-045 | US-2, US-5, US-6 | (secret handling) | `test_secret_redaction_observe`, `test_secret_redaction_ingest`, `test_secret_detected_in_generation` |
| FR-046 | US-10 | Benchmark validates performance envelopes | `test_bench_small_corpus_p1_dev` |
| FR-047 | US-1 | (lock scope) | `test_e2e_concurrent_writers` |
| FR-048 | US-2 | (input sanitization) | `test_prompt_template_sanitization` |
| FR-049 | US-5 | (ingest path safety) | `test_ingest_symlink_outside_root_skipped` |
| FR-050 | US-10 | First-time setup generates Neo4j credentials | `test_neo4j_credential_generation` |
| FR-051 | US-1 | Rebuild fails on model digest drift without --accept-drift | `test_rebuild_uses_pinned_model_metadata`, `test_rebuild_digest_drift_fails` |
| FR-052 | US-6 | Contradiction-only reflection processes open contradictions | `test_reflect_contradictions_only` |
| FR-053 | US-10 | Doctor verifies file permissions | `test_file_permissions_check` |
| FR-054 | US-1, US-9 | (audit trail) | `test_admin_command_audit_identity` |
| FR-055 | US-1 | Recover a torn log tail on startup, Segment quarantine | `test_segment_quarantine_on_checksum_failure`, `test_segment_tx_collision_fails_startup`, `test_segmented_log_merge_sort_on_read` |
| FR-056 | US-1 | (segmented log) | `test_segment_roll_on_size_cap` |
| FR-057 | US-10 | (custom Neo4j+GDS image) | `test_custom_neo4j_gds_image_built_locally` |
| FR-058 | US-10 | Start managed infrastructure, First-time setup generates Neo4j credentials | `test_up_readiness_contract`, `test_up_partial_success_fails` |
| FR-059 | US-10 | (resource allocation) | `test_compose_resource_limits_match_spec` |
| FR-060 | US-10 | (volume topology) | `test_volumes_survive_down_and_purge_flag` |
| FR-061 | US-10 | (upgrade posture) | `test_upgrade_path_export_down_up_rebuild` |
| FR-062 | US-10 | Doctor reports missing dependency details | `test_doctor_host_prerequisites` |

**Completeness check**: Every FR row above maps to at least one BDD scenario and at least one test. Every BDD scenario listed in this spec is represented by at least one test case or supporting dataset row.

---

## Ambiguity Warnings

All ambiguity warnings from the initial draft and from the first implementation-review pass have been resolved. The normative sections of this specification integrate:

- **Round 1 (AMB-W-001 through AMB-W-014)**: datom log concurrency and fsync, commit-vs-index-drift self-healing, frame schema versioning, objective thresholds with rejection observability, PSI namespace governance, module ingestion strategies, contradiction handling, rebuild determinism via datom replay, Neo4j GDS licensing and Louvain fallback, benchmark profiles, model digest identity, flock scope, community detection resolutions, and authoritative hardware profile.
- **Round 2 (implementation review)**: activation datom last-write-wins replay (no compact command needed), segmented log layout at `~/.cortex/log.d/`, sticky `cortex evict` with `cortex unevict` companion, built-in regex secret-detector ruleset, explicit `cortex retract` verb, per-frame reflection watermark advancement with separate scope markers, migrated-content exclusion from cross-project analysis, `cortex doctor --quick`/`--full` split with parallelism, operator-gated real-project value criterion (SC-004a), 20% bound on concurrent-writer lock timeouts, Ollama digest race detection, inline frame type registry, tiny-corpus community detection edge case, segment quarantine and ULID collision handling, and administrative-command audit classification.
- **Round 3 (infrastructure topology)**: pinned container image versions, custom `cortex/neo4j-gds:<release>` image built locally from `docker/neo4j-gds/Dockerfile` with GDS pre-baked, per-service resource allocation (Weaviate 2/4 GB, Neo4j 2/4 GB with specific JVM and GDS budgets), `cortex up` readiness contract with 90-second total budget, named Docker volumes surviving `cortex down`, `cortex down --purge` as the only destructive volume operation, documented `export → down --purge → up → rebuild` upgrade path, host prerequisites enforced by `cortex doctor`, and the change of Weaviate HTTP endpoint from `localhost:8080` to `localhost:9397`.

No open ambiguities remain. Decisions not anticipated by this spec are expected to surface during implementation and should be resolved by updating this document rather than by accumulating implicit choices.

---

## Evaluation Scenarios (Holdout)

Holdout evaluation scenarios are intentionally stored outside this development-facing spec at `/Users/nixlim/Sync/PROJECTS/foundry_zero/knowledge_system/specs/cortex-1/cortex-1-holdouts.md`.

---

## Assumptions

- Phase 1 is a local, single-operator deployment that may host multiple concurrent AI agents on the same machine.
- Ollama, Weaviate, and Neo4j are available locally and are the authoritative Phase 1 dependency set.
- The listed source documents and accepted gate resolutions (round 1 AMB-W-001–014 and the round 2 implementation review) are the authoritative behavioral inputs for this draft.
- Communities with no currently visible members are omitted from default listings rather than being destructively deleted.
- Every entry records `embedding_model_name` and `embedding_model_digest` at write time. Digest is required; fallback to name-only is a degraded mode surfaced by `cortex doctor`.
- The performance targets are validated by `cortex bench` on the authoritative P1-dev profile with a warm stack.
- The datom log is physically segmented at `~/.cortex/log.d/` and logically merged by `tx` ULID at read time.
- Activation datoms use last-write-wins replay semantics, so no compaction command is required in Phase 1.
- Secret detection is performed by the built-in regex ruleset and operator-extension file; no external detector dependency.
- Phase 1's definition of done includes the operator-gated real-project value criterion (SC-004a) in addition to all automated success criteria.
- The Neo4j container image is built locally on each operator's machine from a Dockerfile shipped in the Cortex repository. Cortex does not distribute Neo4j or GDS binaries in pre-built form.
- Per-service resource allocation, volume topology, host prerequisites, and `cortex up` readiness contract are specified in the Infrastructure Topology section and are normative for Phase 1 reproducibility.

---

## Implementer Notes

These are non-normative reminders surfaced during the round-2 implementation review. They do not change any behavioral requirement but they change how Phase 1 must be shipped, monitored, and operated. Implementers should read these before beginning work and again before declaring Phase 1 done.

### Note 1 — SC-004a is load-bearing but cannot be CI-gated

The Phase 1 success criteria include one deliberately manual gate: **SC-004a**, the operator-gated real-project value criterion.

The automated criteria (SC-003 and SC-004) run against fixture projects in CI. Fixtures validate the code path — every pipeline step executes, every edge is emitted, every derived frame is written — but they cannot validate that Cortex delivers useful knowledge on real data, because fixtures are constructed to make the test pass. A system that passes SC-003 and SC-004 against fixtures may still be useless on a real codebase.

**Consequence for whoever ships Phase 1:**

- `cortex bench` exit 0 + all tests passing + SC-001 through SC-017 except SC-004a passing is **necessary but not sufficient** for Phase 1 acceptance.
- Phase 1 is not accepted until the operator has successfully:
  1. Run `cortex ingest` against two of their own real projects (not fixtures, not MemPalace exports),
  2. Run `cortex recall "<cross-cutting concept>"` and observed results drawn from both projects in the top 10,
  3. Run `cortex analyze --find-patterns` and observed at least one `cross_project=true` frame whose `DERIVED_FROM` edges span both projects,
  4. Recorded the validating commit hashes, the recall query, and the resulting cross-project frame ID in `~/.cortex/acceptance.md`.
- The acceptance record is a durable operator artifact. Absence of `~/.cortex/acceptance.md` means Phase 1 is not done, regardless of test status.

CI pipelines cannot assert SC-004a. Whatever process governs Phase 1 acceptance must include a manual human gate that verifies `~/.cortex/acceptance.md` exists and reflects real projects. If that gate does not exist, Phase 1 will ship with passing tests and an unvalidated value proposition — exactly the failure mode the round-2 review was meant to prevent.

### Note 2 — The segmented log changes the backup, restore, and portability model

The round-1 spec assumed a single `~/.cortex/log.jsonl` file. The round-2 spec replaces that with a segment directory at `~/.cortex/log.d/`. This is correct for concurrency and merge semantics but it changes the operational model in ways that are invisible in the spec's normative sections:

- **Backups.** The backup unit is now a directory, not a file. A backup script that copies `log.jsonl` will silently copy *nothing* (the path no longer exists). A backup script that copies `log.d/` must capture every segment file, including anything in `log.d/.quarantine/`, or the restored state will be silently inconsistent. Missing segments do not cause a loud failure on restore; they produce a log with a different merged `tx` set, which the rebuild pipeline will accept as authoritative.
- **Restores.** Restoring from a partial backup is a silent data-loss scenario. A restore must either be complete (all segments from the source directory) or must be paired with a subsequent `cortex merge` of any segments the operator still has available elsewhere.
- **Cross-machine portability.** Moving a Cortex install between machines is no longer "copy one file." It is either (a) `rsync -a ~/.cortex/log.d/ target:~/.cortex/log.d/` (and then `cortex rebuild` on the target), or (b) `cortex export` + `cortex import` using the export path, which serializes the merged stream and is format-independent from the on-disk segment layout.
- **The correct backup command for operators is `cortex export`.** It serializes the merged, `tx`-sorted stream to stdout (or to a file via `--output`) and is insulated from the segment-file physical layout. Phase 1 should document `cortex export` as the supported backup path and `rsync -a ~/.cortex/log.d/` as the supported snapshot path. Any documentation that says "back up `~/.cortex/log.jsonl`" is wrong and must be corrected before Phase 1 ships.
- **Test coverage.** Phase 1's test suite does not include a backup/restore round-trip test. An operator following the documented backup path should be able to restore into a fresh install and reach a consistent state. Adding `test_e2e_backup_restore_roundtrip` is recommended but was not scoped into the round-2 amendment because backup/restore operator UX is not a behavioral requirement of Phase 1 — it is a documentation and tooling concern. Implementers should either add the test or explicitly defer backup/restore documentation to a follow-up.

Neither note changes a behavioral requirement. Both change what "shipping Phase 1" means in practice. Keep them visible to the implementer and to whoever signs off on acceptance.

---

## Clarifications

### 2026-04-09

- Q: How are concurrent writes protected in the datom log? -> A: Use advisory `flock`, `O_APPEND`, one fsync per transaction group, checksum validation, and a 5-second lock timeout.
- Q: What happens when the log commit succeeds but backend apply fails? -> A: The write remains committed, backend drift is self-healed on the next command startup through watermark comparison and replay.
- Q: Are shipped frame types frozen or extensible? -> A: Built-in frame definitions are versioned and stable for Phase 1; users may add custom frame files but may not redefine shipped ones.
- Q: What thresholds govern linking and reflection? -> A: Phase 1 uses the explicit defaults captured in the Configuration Defaults section and exposes dry-run explain output for rejected candidates.
- Q: How are PSI identifiers governed? -> A: Canonical namespace prefixes are mandatory, canonical IDs are immutable, and explicit merge creates aliases accretively.
- Q: How are non-Go and non-Java languages grouped during ingest? -> A: Use the language strategy matrix, then fall back to `per-file` when no language-specific strategy applies.
- Q: What happens when new evidence contradicts existing frames? -> A: Always persist contradiction edges, defer most resolution to scheduled reflection, and immediately scoped re-reflect high-importance contradicted frames.
- Q: How does rebuild remain deterministic with non-deterministic models? -> A: LLM-derived content is stored as datoms at first generation, rebuild replays those datoms, and embeddings use pinned model metadata rather than the current configured model.
- Q: Does ingest automatically run cross-project analysis? -> A: No. `post_ingest_reflect` defaults to true and `post_ingest_analyze` defaults to false unless explicitly enabled.
- Q: What is the lock scope for concurrent writers? -> A: Advisory flock covers only the log append and fsync. All backend writes, LLM calls, and watermark updates execute outside the lock. Backends apply in ULID sort order. Read-dependent commands replay missing datoms before reading.
- Q: How does Neo4j GDS licensing affect Phase 1? -> A: Phase 1 is local-operator use only. Cortex distributes a docker-compose.yaml referencing upstream images. Community detection uses Leiden preferred with automatic Louvain fallback. PPR via gds.pageRank.stream is mandatory.
- Q: Are community detection resolutions locked or tunable? -> A: Level count is frozen at 3 for Phase 1 (schema decision). Resolution values default to [1.0, 0.5, 0.1] and are tunable with startup validation.
- Q: What hardware profile validates performance? -> A: P1-dev (Apple Silicon M-series, 16 GB, NVMe, local stack) is authoritative. P1-ci (GitHub Actions macos-14) is regression-only with a 2x multiplier.
- Q: Is model digest required or optional? -> A: Required. Every entry records embedding_model_name and embedding_model_digest. Rebuilds fail if the pinned digest is unavailable unless --accept-drift is passed.

### 2026-04-09 (round 2)

- Q: How should Cortex handle activation-datom growth over the life of the log? -> A: Last-write-wins replay semantics. Activation datoms continue to be written to the log, but replay collapses to the final activation per `(entity, attribute)`. No compact command is required.
- Q: How is the datom log physically organized under concurrent writers and merges? -> A: Segmented at `~/.cortex/log.d/`. Each writer owns a segment file; segments roll at `log.segment_max_size_mb` (default 64 MB). Readers merge-sort by `tx` ULID. Merge drops foreign segments in verbatim after checksum validation.
- Q: Is `cortex evict` sticky or one-shot? -> A: Sticky. `evict` writes a sticky `evicted_at` attribute that blocks reinforcement across all retrieval modes until an explicit `cortex unevict` (or `cortex pin`) retracts it.
- Q: Which secret detector does Phase 1 use? -> A: A built-in regex ruleset embedded in the binary, covering AWS/GCP/Azure credentials, GitHub/GitLab/Slack/Discord tokens, PEM private keys, JWTs, and generic high-entropy secret-adjacent patterns. Operators extend via `~/.cortex/secrets.yaml`. No external dependency.
- Q: How is `cortex retract` scoped and shaped? -> A: `cortex retract <id> [--cascade] [--reason=<text>]` is an operator-scope command that accepts any entity type (entry, frame, trail, subject, community), writes retraction datoms, records invoking identity and reason as audit datoms, and preserves full lineage in `cortex history`.
- Q: How does the reflection watermark behave across interrupted runs and scoped reflections? -> A: The scheduled-reflection watermark advances per accepted frame, so an interrupted run resumes without reprocessing already-written frames. Scoped reflections (post-ingest, `--contradictions-only`, `--project=`) use separate markers and do not advance the global watermark.
- Q: Are migrated MemPalace entries included in cross-project analysis? -> A: Excluded by default. Migrated content is marked `migrated=true` and skipped by `cortex analyze --find-patterns` unless the operator passes `--include-migrated`.
- Q: Does `cortex doctor` have a time budget? -> A: `cortex doctor --quick` completes in under 5 seconds and runs only checks with individual execution time bounded under 500 ms. `cortex doctor --full` (default) runs all checks with `doctor.parallelism=4` workers.
- Q: How is the "cross-project value criterion" validated? -> A: Two success criteria. SC-003 and SC-004 are automated CI checks against fixture projects. SC-004a is an operator-gated criterion requiring successful ingestion of two of the operator's own real projects and a recorded acceptance entry in `~/.cortex/acceptance.md`. SC-004a is load-bearing; Phase 1 is not complete without it.
- Q: What bound applies to lock-timeout failures under concurrent writers? -> A: No more than 20% of concurrent writers may fail with `LOCK_TIMEOUT` under the 50-writer contention test (SC-007).
- Q: How is the Ollama digest race handled? -> A: Cortex caches the digest from a single `ollama show` call at write start and verifies it against the embedding response. A mismatch aborts the write with `MODEL_DIGEST_RACE` (exit 1). The operator re-runs after Ollama settles.
- Q: What happens when the corpus is too small for community detection to produce meaningful clusters? -> A: Leiden/Louvain runs normally and produces zero or one trivial community. `cortex communities` returns an empty list; `--mode=community` returns empty; reflection does not fail.
- Q: Which frame types are agent-writable and which are reflection-only? -> A: Three episodic types are agent-writable (`Observation`, `SessionReflection`, `ObservedRace`). The remaining eight frame types are reflection-only. Direct `cortex observe --kind=<reflection-only-type>` is rejected with a validation error.

### 2026-04-09 (round 3 — infrastructure topology)

- Q: How is the Neo4j Graph Data Science plugin installed? -> A: Phase 1 ships a custom Neo4j image (`cortex/neo4j-gds:<cortex-release-version>`) with GDS pre-baked. The Dockerfile lives at `docker/neo4j-gds/Dockerfile` in the Cortex repository. `cortex up` builds the image locally on first run; Cortex does not push or distribute pre-built image binaries.
- Q: What is the authoritative Weaviate HTTP endpoint? -> A: `localhost:9397` (changed from the prior `localhost:8080` default). gRPC remains at `localhost:50051`.
- Q: What resource allocation does `docker-compose.yaml` declare? -> A: Weaviate 2 GB request / 4 GB limit; Neo4j 2 GB request / 4 GB limit with JVM heap `-Xms1G -Xmx2G`, page cache 1 GB, GDS budget 1 GB. Host RAM floor is 12 GB (enforced by `cortex doctor`), recommended 16 GB.
- Q: What is the `cortex up` readiness contract? -> A: Docker daemon reachable, ordered service start (Weaviate then Neo4j), per-service probes (Weaviate `/v1/.well-known/ready`, Neo4j Bolt + `gds.version()`, Ollama `/api/tags` with required models present), 90-second total wall-clock budget, all-or-nothing outcome.
- Q: What is the Phase 1 backup unit and upgrade path? -> A: The backup unit is the datom log at `~/.cortex/log.d/` (via `cortex export` or `rsync -a`). Managed-service volumes are derived and not required for backup. The upgrade path is `export → down --purge → install new release → up → rebuild`; there is no in-place upgrade in Phase 1.
- Q: Where do Weaviate and Neo4j persist state? -> A: Named Docker volumes `cortex_weaviate_data` and `cortex_neo4j_data`. Volumes survive `cortex down`; only `cortex down --purge` removes them after operator confirmation.
- Q: How does Cortex detect GDS version and algorithm availability? -> A: `cortex doctor` calls `gds.version()` and probes each required procedure (`gds.pageRank.stream`, `gds.graph.project`, and either `gds.leiden.stream` or `gds.louvain.stream`). The Leiden-preferred / Louvain-fallback logic is keyed off callable procedures, not version strings.
- Q: What host prerequisites are enforced? -> A: macOS `darwin/arm64`, Docker `>= 4.25` with Compose v2, 12 GB RAM floor, 10 GB free disk on `~/.cortex/`, ports 9397/50051/7474/7687 free, `ulimit -n >= 4096`, Ollama `>= 0.1.40` with both required models pulled. `cortex up` refuses to start on hard-fail prerequisites.
