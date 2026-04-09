# Phase 1 Spec (v2) — Cortex Knowledge System

Supersedes `phase-1-spec.md`.
Implements `architecture-v2.md`.
Grounded in `research-report.md`.

## Goal

Build a Go CLI (`cortex`) backed by Weaviate (vector) + Neo4j (graph) in Docker, organized as a two-memory-system store (episodic / semantic / procedural) with datoms as the storage substrate, typed Minsky frames for structured notes, Ranganathan faceted classification, Memex trails as first-class entities, write-time derived linking, HippoRAG retrieval, ACT-R-style forgetting, **full code-ingestion pipeline**, and **cross-project pattern analysis**.

Replaces MemPalace with a system designed for multi-agent concurrent use, cross-project knowledge discovery, replayable accretive storage, consolidation from episodes to patterns, and principled forgetting. Phase 1 ships the complete system; only visualization is deferred.

## Non-Goals (Phase 1)

- Knowledge graph visualization UI (Phase 2).
- Multi-user / multi-tenant support.
- Web UI or API server beyond what Weaviate and Neo4j expose natively.
- Cross-device sync (enabled by the datom log but out of scope for Phase 1).

---

## Design Decisions (Resolved)

| Decision | Choice | Rationale |
|---|---|---|
| Organizing metaphor | **CoALA three-store partition + Datomic substrate.** | Zettelkasten and Method of Loci are human techniques. CoALA + CLS is the cognitive-science-informed model for agent memory; Datomic gives event-sourcing as a primitive. See `research-report.md`. |
| Storage atom | **Immutable datom `(E, A, V, Tx, Agent, Confidence)`.** | Event log and store are the same thing. Merge, replay, time-travel become substrate primitives. |
| Note shape (semantic / procedural) | **Typed Minsky frame with named slots; each slot value is a Wikidata-style claim with qualifiers and references.** | Frames are machine-comparable across projects. Claims carry provenance natively. LLMs fill slots reliably. |
| Note shape (episodic) | **Lightweight: body text, kind, facets, trail reference, provenance.** | Episodic writes must be cheap. |
| Classification | **Ranganathan facets (Kind, Domain, Artifact, Project, Time, Language) — mandatory on every entry.** | Faceted queries handle most of what the v1 spec called "topics," compose orthogonally, and require no hierarchy. |
| Linking | **Derived, never authored. Three mechanisms: A-MEM write-time LLM derivation; Topic-Maps PSI merge; GraphRAG hierarchical Leiden communities with LLM summaries.** | Mandatory authored linking produces hairballs. Move the work to the substrate. |
| Topics | **Hierarchical Leiden communities with LLM-written summaries at each level, plus faceted queries.** | Communities gain names and meaning. Multiple resolutions enable zoom. |
| Sequences | **Memex trails as first-class entities.** | Strictly more expressive than Zettelkasten sequences: named, ordered, replayable, attachable to entries, produced automatically from sessions. |
| Retrieval | **HippoRAG Personalized PageRank seeded from LLM-extracted query concepts, reranked by ACT-R activation (base-level + spreading + similarity + importance).** | Single operation; both intentional traversal and serendipity; cheaper than iterative RAG; accounts for recency and use. |
| Write to semantic store | **Only via scheduled reflection / consolidation job.** | Complementary Learning Systems: the hippocampus does fast episodic encoding; the neocortex consolidates regularities via replay. Agents write episodic; the substrate writes semantic. |
| Forgetting | **ACT-R base-level activation (power-law decay) + reinforcement on retrieval. Nothing is ever deleted; stale entries fall out of default rankings.** | Long-lived knowledge stores need forgetting or they drown in their own accumulation. |
| Dedup | **PSI-based identity merge where an identifier exists; LLM judgement at write time otherwise.** | The v1 fixed-threshold approach risks silent loss. PSI merge is a principled, predictable operation. |
| Diary | **Subsumed into episodic store with a reserved frame (`SessionReflection`).** | One less concept. Session diaries are just episodic entries with a specific kind. |
| Phase split | **Collapsed. Phase 1 ships ingestion and cross-project analysis; only visualization is deferred to Phase 2.** | Reflection and the value proposition both require real cross-project episodic content. Deferring ingestion left Phase 1 unable to demonstrate or validate its own value. The value criterion is load-bearing; capability checks alone do not constitute completion. |
| AAAK / identity.txt | **Dropped.** | Presentation-layer concerns, not storage. |
| Wings / rooms | **Dropped.** | Replaced by facets, which are strictly better. |

---

## Infrastructure

### Docker Compose

```yaml
services:
  weaviate:
    image: cr.weaviate.io/semitechnologies/weaviate:1.36.9
    ports:
      - "8080:8080"
      - "50051:50051"
    volumes:
      - cortex_weaviate:/var/lib/weaviate
    environment:
      QUERY_DEFAULTS_LIMIT: 25
      AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED: "true"
      PERSISTENCE_DATA_PATH: "/var/lib/weaviate"
      DEFAULT_VECTORIZER_MODULE: "text2vec-ollama"
      ENABLE_MODULES: "text2vec-ollama,generative-ollama"
      CLUSTER_HOSTNAME: "node1"
    restart: unless-stopped

  neo4j:
    image: neo4j:5-community
    ports:
      - "7474:7474"
      - "7687:7687"
    volumes:
      - cortex_neo4j:/data
    environment:
      NEO4J_AUTH: neo4j/cortex-local
      NEO4J_PLUGINS: '["graph-data-science"]'
    restart: unless-stopped

volumes:
  cortex_weaviate:
  cortex_neo4j:
```

Ollama is assumed running on the host at `localhost:11434` and is not managed by this compose file.

### Resource Requirements

| Service | RAM | Disk | Notes |
|---|---|---|---|
| Weaviate | 1–2 GB | scales with data | HNSW index memory-resident |
| Neo4j Community | 1–2 GB | scales with data | JVM heap, GDS plugin for PPR and Leiden |
| Ollama | varies | model-dependent | shared service; not exclusive to Cortex |
| **Total** | **~3–4 GB** + Ollama | | manageable on a 16 GB+ machine |

The Neo4j Graph Data Science plugin is mandatory — PPR and Leiden are implemented by it and are load-bearing for retrieval and topic derivation.

---

## Data Model

### The Datom

Everything in Cortex is a datom. Format:

```json
{
  "tx":     "01JREXXXXXXXXXXXXXXXXXXXXX",   // ULID, monotonic within a writer
  "e":      "entry:01JREYYYYYYYYYYYYYYYYYYYYY",
  "a":      "content",
  "v":      "API has TOCTOU race in token validation ...",
  "op":     "assert",                        // or "retract"
  "agent":  "grill-spec-agent",
  "confidence": 0.9,
  "valid_from": "2026-04-09T12:34:56Z",
  "valid_to":   null
}
```

- Entities are identified by namespaced IDs (`entry:...`, `trail:...`, `community:...`, `frame:...`, `subject:...`).
- Attributes are a fixed, extensible vocabulary declared in `~/.cortex/schema.edn` (or equivalent).
- Retractions are themselves datoms, never in-place mutations.
- Transaction IDs are ULIDs — globally unique, temporally ordered, mergeable without a central clock.

### Datom Log

Stored at `~/.cortex/log.jsonl`. One JSON datom per line. Append-only. This file is the source of truth. Weaviate and Neo4j are derived indexes and can be rebuilt at any time by replaying the log.

The log is portable: `cat`, `jq`, `rsync`, `git`, email — all work. Merge is set union with deduplication by `tx`. Rebuild is `cortex rebuild`, which truncates both backends and replays.

### Entities and Frames

#### Entry (episodic)

```
Entity: entry:<ulid>
Attributes:
  kind              "Observation" | "SessionReflection" | "Trace" | ...
  body              text
  facet/kind        one of the Kind facet values
  facet/domain      one of the Domain facet values
  facet/artifact    one of the Artifact facet values
  facet/project     canonical project ID
  facet/time        valid-time of the observation
  facet/language    programming language (optional)
  trail             trail:<ulid>
  source            agent name or "human"
  source_ref        file / commit / context
  embedding_model   name of the embedding model used at write time
  base_activation   current ACT-R base-level activation (updated by retrieval)
  last_retrieved_at timestamp
  retrieval_count   integer
```

#### Entry (semantic / procedural) — a frame

Semantic and procedural entries are typed Minsky frames. Each frame type declares required and optional slots. Example:

```
Frame: BugPattern
Required slots:
  name               short canonical name
  symptom            observable behavior
  root_cause         mechanistic explanation
  conditions         when it occurs
  remediation        how to fix / prevent
Optional slots:
  exemplars          list of references to episodic entries
  related_patterns   list of references to other frames (PSI-identified)
  first_seen         earliest valid-time
  last_seen          latest valid-time
```

Slot values are **claims**:

```json
{
  "slot": "root_cause",
  "value": "Token validation and resource access are not atomic; ...",
  "qualifiers": {
    "valid_from": "2026-04-09",
    "project": "payment-gateway",
    "confidence": 0.85
  },
  "references": [
    "entry:01JREXXXXX...",
    "entry:01JRFXXXXX..."
  ],
  "asserted_by": "reflection-job",
  "asserted_at": "2026-04-10T03:00:00Z"
}
```

Frame types shipped in Phase 1:

- `Observation` (episodic-only)
- `SessionReflection` (episodic-only; absorbs the v1 diary)
- `BugPattern`
- `DesignDecision`
- `RetryPattern`
- `ReliabilityPattern`
- `SecurityPattern`
- `LibraryBehavior`
- `Principle`
- `ObservedRace`
- `ArchitectureNote`

Frames are declared in `~/.cortex/frames/*.json` and can be extended by the user. Cortex validates writes against the frame schema.

#### Trail

```
Entity: trail:<ulid>
Attributes:
  name        short name
  agent       agent that produced it
  started_at  timestamp
  ended_at    timestamp
  facet/project
  facet/domain
  summary     LLM-written trail summary (filled at end)
  entries     ordered list of entry:<ulid>
  outcomes    list of references to semantic frames produced
```

Trails are produced by every agent session. `cortex trail begin` at session start; `cortex trail end` at session end (mandatory, like the current MemPalace diary rule). Between them, every `cortex observe` call attaches to the current trail.

#### Community

```
Entity: community:<ulid>
Attributes:
  level        integer (Leiden hierarchy level)
  parent       community:<ulid> | null
  members      list of entry/frame IDs
  summary      LLM-written summary
  size         member count
  refreshed_at timestamp of last Leiden run
```

Communities are produced by the reflection job. They are first-class entities because agents navigate to them via `cortex community` and because summaries are the queryable "topic" in the new model.

#### Subject (PSI)

```
Entity: subject:<canonical-identifier>
Attributes:
  name         human-readable
  aliases      list of alternate names
  kind         "Library" | "CVE" | "BugClass" | "ADR" | ...
  unifies      list of entry/frame IDs that refer to the same subject
```

PSI subjects are shared across projects. Writing an observation about the `nomic-embed-text` library looks up or creates `subject:lib/nomic-embed-text` and attaches the observation to it. Merges happen along PSI edges.

#### Project

```
Entity: project:<canonical-name>
Attributes:
  name                canonical project identifier (also used as the facet/project value)
  path                filesystem path last used for ingestion
  remote              git remote URL, if any
  last_ingested_at    timestamp of most recent ingest
  last_ingested_sha   git commit SHA at last ingest (for incremental re-ingestion)
  include_globs       list of path globs to include
  exclude_globs       list of path globs to exclude (defaults include .gitignore + build dirs)
  language_hints      dominant languages detected during walk
  module_strategy     module-grouping strategy used ("per-file", "per-package", ...)
```

A `:Project` node is created or updated on every `cortex ingest`. The project ID is the single source of truth for project identity — it is also the value used in `facet/project` on every entry, trail, and frame derived from that codebase.

### Derived Indexes

#### Weaviate Collections

```
Class: Entry
  Properties: id, body, kind, domain, artifact, project, language,
              valid_from, valid_to, trail_id, source, base_activation
  Vectorizer: text2vec-ollama on body
```

```
Class: Frame
  Properties: id, frame_type, canonical_text (flattened slot values),
              kind, domain, artifact, project, language
  Vectorizer: text2vec-ollama on canonical_text
```

Both collections are rebuilt from the datom log.

#### Neo4j Schema

Nodes:

```cypher
(:Entry {id, kind, project, domain, artifact, language, valid_from, base_activation, last_retrieved_at})
(:Frame {id, frame_type, project, domain, artifact, language, valid_from, base_activation, last_retrieved_at, cross_project})
(:Trail {id, name, agent, started_at, ended_at, project, domain})
(:Community {id, level, parent, size, refreshed_at})
(:Subject {id, name, kind})
(:Project {id, name, remote, last_ingested_sha, last_ingested_at, module_strategy})
```

Edges (all derived; none authored by agents):

```cypher
(:Entry|:Frame)-[:IN_TRAIL {order: int}]->(:Trail)
(:Entry|:Frame|:Trail)-[:FROM_PROJECT]->(:Project)
(:Entry|:Frame)-[:ABOUT]->(:Subject)
(:Entry|:Frame)-[:MEMBER_OF]->(:Community)
(:Community)-[:PARENT_OF]->(:Community)
(:Frame)-[:REFERENCES {slot: string}]->(:Entry|:Frame)
(:Frame)-[:DERIVED_FROM]->(:Entry|:Frame)     // reflection lineage
(:Frame)-[:SAME_SUBJECT]->(:Frame)            // PSI merge
(:Frame)-[:CONTRADICTS {reason: string}]->(:Frame)
(:Frame)-[:SUPERSEDES]->(:Frame)
(:Frame)-[:EXEMPLIFIES]->(:Frame)
(:Entry|:Frame)-[:SIMILAR_TO {score: float, derived_by: string}]->(:Entry|:Frame)
```

Constraints and indexes:

```cypher
CREATE CONSTRAINT entry_id FOR (e:Entry) REQUIRE e.id IS UNIQUE;
CREATE CONSTRAINT frame_id FOR (f:Frame) REQUIRE f.id IS UNIQUE;
CREATE CONSTRAINT trail_id FOR (t:Trail) REQUIRE t.id IS UNIQUE;
CREATE CONSTRAINT community_id FOR (c:Community) REQUIRE c.id IS UNIQUE;
CREATE CONSTRAINT subject_id FOR (s:Subject) REQUIRE s.id IS UNIQUE;
CREATE CONSTRAINT project_id FOR (p:Project) REQUIRE p.id IS UNIQUE;

CREATE INDEX entry_project FOR (e:Entry) ON (e.project);
CREATE INDEX frame_type FOR (f:Frame) ON (f.frame_type);
CREATE INDEX entry_activation FOR (e:Entry) ON (e.base_activation);
CREATE INDEX frame_activation FOR (f:Frame) ON (f.base_activation);

CREATE FULLTEXT INDEX entry_body FOR (e:Entry) ON EACH [e.body];
CREATE FULLTEXT INDEX frame_text FOR (f:Frame) ON EACH [f.canonical_text];
```

---

## Write Pipeline

### Episodic write (`cortex observe`)

1. Parse and validate: body, kind, facets, optional frame-type, optional `--trail` (defaults to current session trail).
2. Append datoms to the log: one for each attribute of the new entry.
3. Apply to Neo4j: create `:Entry` node, `:IN_TRAIL` edge, `:ABOUT` edges to any resolved `:Subject` nodes.
4. Apply to Weaviate: insert document, letting Ollama embed the body.
5. **Derived linking (A-MEM style).** Retrieve top-K nearest neighbors from Weaviate. Prompt an LLM with the new entry plus its neighbors; the LLM returns a small number of typed links (`SAME_SUBJECT`, `EXEMPLIFIES`, `CONTRADICTS`, `SIMILAR_TO`) with a rationale. Write those as datoms. Optionally, the LLM may update the `context` attribute of neighboring entries; those updates are also datoms.
6. **PSI resolution.** If the entry refers to a canonical subject (via `--subject` flag or LLM-detected entity mention), create or locate `:Subject` and attach via `:ABOUT`.
7. Return the new entry ID.

All steps 2–6 happen inside a single transaction group (shared `tx` ID). Failure rolls back the in-memory index application; the log write is the commit point.

### Reflection / consolidation (`cortex reflect`)

Scheduled job, runnable manually. Reads episodic memory and writes semantic frames.

1. **Candidate selection.** Query recent episodic entries (since last reflection). Cluster by concept (k-means on embeddings or agglomerative on PPR neighborhoods) and by facet overlap.
2. **Frame proposal.** For each cluster of sufficient size, prompt an LLM: "here are N observations — do they exemplify a reusable pattern? If so, produce a frame of type T with filled slots." The LLM picks a frame type or declines.
3. **Compression check (MDL-flavored).** The proposed frame is accepted iff (a) its canonical text compresses the joint content of its exemplars by a configurable ratio, and (b) it generalizes beyond a single project (or is explicitly marked project-specific).
4. **Write frame.** Emit datoms for the new `:Frame` with slot claims, `DERIVED_FROM` edges to exemplar entries, and facet metadata inherited from the exemplars' dominant facets.
5. **Supersession check.** If a new frame is similar to an existing frame and is strictly more general, emit a `SUPERSEDES` edge. The superseded frame is not deleted — its base activation decays naturally.
6. **Community refresh.** After frame writes, run Leiden clustering over the semantic graph. For each new or changed community, prompt an LLM to write a fresh summary. Emit community datoms.

Reflection is the only path into the semantic store. Agents do not write frames directly.

### Trail lifecycle

- `cortex trail begin --agent=<name> [--name=<label>]` creates a new trail. Returns a trail ID that the agent exports as `CORTEX_TRAIL_ID`.
- Every subsequent `cortex observe` attaches to the current trail.
- `cortex trail end` writes a trail summary (LLM-generated from the entries) and marks `ended_at`. This is mandatory at session end.
- Trails can be recalled, walked, and used as similarity seeds.

---

## Retrieval Pipeline

The default `cortex recall "<query>"` path (HippoRAG PPR + ACT-R rerank):

1. **Concept extraction.** Prompt the LLM: "Extract canonical concept mentions from this query." Returns a list of concept strings.
2. **Seed resolution.** For each concept, find matching `:Subject` nodes (via PSI) and matching `:Entry|:Frame` nodes (via vector search). Collect as the PPR seed set with weights.
3. **Personalized PageRank.** Run Neo4j GDS `gds.pageRank.stream` with the seed set as `sourceNodes`. Produces a ranked list of candidate entries and frames.
4. **ACT-R rerank.** For each candidate, compute:
   ```
   activation = w_b * base_level(recency, frequency)
              + w_s * ppr_score
              + w_v * embedding_similarity(query, entry)
              + w_i * importance(frame_type, facet_weights)
   ```
   Weights are configurable; defaults derived from ACT-R literature and Park et al.'s Generative Agents work.
5. **Assemble result.** Return the top-N entries and frames, each annotated with:
   - Their owning trail and trail summary.
   - Their community membership at multiple levels, with community summaries.
   - A "why surfaced" trace (which seed, which PPR path, which activation components dominated).
6. **Reinforcement.** For each returned entry, emit an activation update datom: `base_activation ← reinforce(base_activation, now())` and bump `retrieval_count`, `last_retrieved_at`. These are regular datoms subject to replay.

Alternate retrieval modes (`--mode=`):
- `similar` — pure vector similarity (Weaviate).
- `traverse` — multi-hop graph walk from a given entry (Neo4j, bounded by depth).
- `path` — shortest path between two entries (Neo4j GDS `shortestPath`).
- `community` — retrieve a community's summary and top members at a given level.
- `surprise` — random walk weighted by `(1 − recency)`, to surface stale or under-used knowledge.

---

## Forgetting

Every entry and frame carries a `base_activation` attribute updated by datoms, not in-place mutation. The ACT-R base-level equation:

```
b(t) = ln(Σ_i (t − t_i)^(−d))
```

where `t_i` are the timestamps of prior retrievals and `d` is a configurable decay exponent (default 0.5). In practice this is approximated incrementally on each retrieval rather than recomputed from scratch.

Entries with activation below a configurable threshold are **not deleted** but drop out of the default retrieval top-N. They remain in the datom log and can be surfaced by `cortex recall --mode=surprise` or by explicit subject / trail lookup. This preserves accretion while ensuring the steady-state retrieval is driven by currently-useful knowledge.

Stale **communities** are refreshed on the next reflection run.

---

## Ingestion Pipeline

`cortex ingest <project-path>` is a first-class write path on equal footing with `cortex observe`. It turns a codebase into episodic entries by summarizing and analyzing modules through Ollama, then triggers a targeted reflection pass to promote recurring patterns. It is the highest-volume write path in Cortex and the main source of cross-project signal.

### Walk and filter

1. **Resolve project identity.** Read `.cortex/project.yaml` from the target path if present, else derive a canonical ID from the git remote URL (or, failing that, the directory name). Create or update the `:Project` node and its datoms.
2. **Walk the file tree.** Honor `.gitignore` by default; apply configured include/exclude globs; skip binary files, vendored dependencies, and build output directories (`vendor/`, `node_modules/`, `dist/`, `build/`, `.git/`, etc.).
3. **Group into modules.** A module is the smallest unit Cortex writes an entry for. Default rule: one module per source file for most languages, one per package for Go, one per class for Java/Kotlin. Grouping strategies are pluggable via `~/.cortex/ingest/strategies/*.yaml`.
4. **Incremental diff.** On re-ingest, compare against `last_ingested_sha` and process only modules touched by changed files. New files become new modules. **Deleted files cause their prior entries to be retracted via datoms** — retraction is itself a fact, accretive rather than destructive, so history is preserved.

### Per-module processing

For each module, Ollama is prompted to produce a structured summary:

```json
{
  "summary": "2-4 sentence description of what the module does",
  "responsibilities": ["discrete responsibility 1", "..."],
  "dependencies": ["imported libraries / internal packages"],
  "patterns_detected": [
    {"kind": "RetryPattern", "evidence": "...", "confidence": 0.8},
    {"kind": "ErrorHandling", "evidence": "..."}
  ],
  "decisions_inferred": [
    {"summary": "chose X over Y because...", "evidence": "..."}
  ],
  "risks": ["flagged smells or hazards"]
}
```

From this structured output, the ingester emits:

- **One `Observation` entry per module** carrying `summary` and `responsibilities` as the body. Facets: `Kind=Summary`, `Domain` inferred from detected patterns, `Artifact=Module`, `Project`, `Language`, `Time=now`.
- **One pattern observation per `patterns_detected` item** with the corresponding `Kind` facet (`RetryPattern`, `ErrorHandling`, etc.) and the evidence text as body. These are episodic observations, not frames — reflection decides whether they cohere into frames.
- **One decision observation per `decisions_inferred` item** with `Kind=DesignDecisionHint`, to be promoted or discarded by reflection.
- **PSI resolutions** for each dependency: look up or create `subject:lib/<name>` and attach the module entry via `:ABOUT`. This is where cross-project signal gets its spine — the same library seen across multiple projects becomes a subject hub, which reflection and cross-project analysis use to cluster cross-project episodes.

Every entry goes through the **standard write pipeline**. Write-time link derivation runs per entry, so ingested entries are linked into the existing graph as they land, not in a separate pass.

### Trail per ingest

Each `cortex ingest` invocation runs inside a synthesized trail: `trail:ingest:<project>:<timestamp>`. All module observations, pattern hits, and decision hints attach to this trail. Re-ingests produce new trails, and the ingest history is queryable via `cortex trail list --project=<name>`. Failed ingests leave a partial trail that can be resumed — the ingester is idempotent under the datom log.

### Post-ingest reflection

After all module writes complete, the ingester runs a targeted reflection pass: `cortex reflect --since=<ingest-start> --project=<name>`. This is the consolidation that turns ingested episodes into frames while the context is hot. Unlike scheduled reflection, the post-ingest pass is scoped to the just-ingested project and its immediate neighbors in subject space (projects sharing libraries, CVEs, or bug classes).

### Throughput and cost

Ingestion is the most expensive write path: one LLM call per module plus one embedding call per entry plus write-time link derivation per entry. Configurable Ollama concurrency in `config.yaml` throttles the pipeline. `cortex doctor` validates that Ollama can sustain the configured concurrency before a large ingest starts.

---

## Cross-Project Analysis

`cortex analyze --find-patterns [--projects=A,B,C] [--since=<time>]` is an unrestricted consolidation pass across multiple projects, designed to surface patterns that are only visible in aggregate. It shares the reflection pipeline but with different selection criteria and scoring weights.

### Differences from scheduled reflection

| Aspect | Scheduled reflection | `cortex analyze --find-patterns` |
|---|---|---|
| Time window | Since last run | Unrestricted by default |
| Project scope | All projects | Configurable subset; defaults to all |
| Candidate selection | Recent episodic clusters | Cross-project clusters — episodes from ≥2 projects required |
| Compression threshold | Standard MDL ratio | Relaxed; cross-project signal is rare and worth preserving at lower ratios |
| Frame bias | Any frame type | Biased toward `BugPattern`, `RetryPattern`, `ReliabilityPattern`, `SecurityPattern`, `Principle` — frames whose value is highest when cross-project |
| Community refresh | Incremental | Full Leiden re-run over the semantic graph |
| Frame marking | normal | `cross_project=true` on accepted frames |

### Pipeline

1. **Cluster selection.** Group episodic entries by (a) shared PSI subjects across projects, (b) shared facet signatures across projects, and (c) embedding proximity across projects. Each cluster must draw from at least two distinct `facet/project` values.
2. **Frame proposal.** For each cluster, prompt the LLM: "These observations come from multiple projects. Do they exemplify a reusable cross-project pattern? If so, produce a frame of the best-fitting type with filled slots." The prompt is explicit about the cross-project constraint and provides the projects' names and facet context.
3. **Relaxed MDL check.** The proposed frame must still compress the joint content of its exemplars, but the threshold is lower than scheduled reflection.
4. **Write frame.** Emit datoms for the new `:Frame` with slot claims and `DERIVED_FROM` edges spanning the exemplar projects. Set `cross_project=true`, which raises the frame's importance weight in ACT-R activation and makes it surface more readily in retrieval.
5. **Full community refresh.** Run a full Leiden re-run over the semantic graph. Refresh all community summaries whose membership changed.
6. **Report.** `--dry-run` mode prints the plan without writing. Default mode reports: N clusters considered, N frames proposed, N frames accepted, N communities changed, N subjects gained cross-project frames.

### When to run

- **After ingesting a new project** — trigger a run to fold the new project into the cross-project structure. The ingester can do this automatically via `ingest.post_analyze: true`.
- **On a slower schedule than reflection** — default weekly.
- **On demand** — when a user wants to surface patterns across a specific project subset.

---

## CLI Interface

```
cortex — Cognitive-architecture-backed knowledge system

USAGE:
    cortex <command> [flags]

EPISODIC WRITES:
    cortex observe "body" --kind=<FrameType> --facets=k1:v1,k2:v2 [--trail=<id>] [--subject=<psi>]
                                           Append an episodic observation.
    cortex trail begin --agent=<name> [--name=<label>]
                                           Start a session trail.
    cortex trail end [--id=<trail-id>]     Finalize and summarize a trail.
    cortex trail show <id>                 Display a trail.
    cortex trail list [--agent=] [--project=] [--since=]

RECALL / RETRIEVAL:
    cortex recall "query" [--mode=default|similar|traverse|path|community|surprise]
                          [--limit=N] [--facets=...] [--json]
    cortex get <id>                        Retrieve a specific entry or frame.
    cortex neighbors <id> [--depth=1]
    cortex path <id-a> <id-b>

REFLECTION / CONSOLIDATION:
    cortex reflect [--since=<time>] [--project=] [--dry-run]
                                           Run episodic → semantic consolidation.
    cortex communities [--level=N]         List communities at a Leiden level.
    cortex community show <id>             Show community summary + members.

INGESTION:
    cortex ingest <path> [--project=<name>] [--dry-run]
                         [--include=<glob>...] [--exclude=<glob>...]
                         [--strategy=per-file|per-package|...]
                         [--no-post-reflect]
                                           Walk a codebase and write episodic entries.
    cortex ingest status [--project=<name>]
                                           Show last-ingested SHA, timestamp, and counts.
    cortex ingest resume --project=<name>  Resume a partial ingest from the datom log.
    cortex project list
    cortex project show <name>
    cortex project forget <name>           Retract all entries from a project (accretive;
                                           nothing is deleted from the log).

CROSS-PROJECT ANALYSIS:
    cortex analyze [--find-patterns] [--projects=A,B,...]
                   [--since=<time>] [--dry-run]
                                           Unrestricted consolidation across projects.

SUBJECTS (PSI):
    cortex subject add <psi> --name=<name> --kind=<kind>
    cortex subject show <psi>
    cortex subject merge <psi-a> <psi-b>   Explicitly merge two subjects.

PINNING / EVICTION (MemGPT-style):
    cortex pin <id>                        Protect from activation decay.
    cortex unpin <id>
    cortex evict <id>                      Force low activation (not deletion).

DATA MANAGEMENT:
    cortex merge <log-file>                Merge another cortex's datom log.
    cortex export [--since=<tx>]           Export datoms to stdout.
    cortex rebuild                         Rebuild Weaviate + Neo4j from the log.
    cortex backup [--dir=<path>]           Backup the log + DB snapshots.
    cortex migrate --from-mempalace=<path>
    cortex history <id>                    Show all datoms for an entity.
    cortex as-of <tx-id> <subcommand>      Run a query as of a point in time.

INFRASTRUCTURE:
    cortex up | down | status | doctor
    cortex config show | set <key> <value>
    cortex frames list | show <type> | validate <file>

GLOBAL FLAGS:
    --json          Machine-readable output (mandatory for agent use).
    --verbose
    --quiet
```

**Absent verbs:** `link`, `unlink`. Linking is not an authoring operation in Cortex.

---

## Configuration

```yaml
# ~/.cortex/config.yaml

weaviate:
  endpoint: "http://localhost:8080"
  grpc_endpoint: "localhost:50051"

neo4j:
  uri: "bolt://localhost:7687"
  username: "neo4j"
  password: "cortex-local"
  database: "neo4j"

ollama:
  endpoint: "http://localhost:11434"
  embedding_model: "nomic-embed-text"
  concept_extraction_model: "llama3.1:8b-instruct"
  reflection_model: "llama3.1:8b-instruct"
  link_derivation_model: "llama3.1:8b-instruct"

retrieval:
  default_mode: "default"
  default_limit: 10
  hybrid_alpha: 0.7
  ppr:
    damping: 0.85
    max_iterations: 20
    seed_top_k: 5
  activation:
    decay_exponent: 0.5
    weights:
      base_level: 0.3
      ppr: 0.3
      similarity: 0.3
      importance: 0.1
  forgetting:
    visibility_threshold: 0.05

reflection:
  schedule: "0 3 * * *"    # daily at 03:00
  min_cluster_size: 3
  mdl_compression_ratio: 1.3
  community_detection:
    algorithm: "leiden"
    levels: 3

ingest:
  strategies_dir: "~/.cortex/ingest/strategies"
  default_include:
    - "**/*.go"
    - "**/*.py"
    - "**/*.js"
    - "**/*.ts"
    - "**/*.rs"
    - "**/*.java"
    - "**/*.kt"
    - "**/*.rb"
    - "**/*.cs"
  default_exclude:
    - "**/vendor/**"
    - "**/node_modules/**"
    - "**/dist/**"
    - "**/build/**"
    - "**/.git/**"
    - "**/*.generated.*"
  module_strategy: "per-file"           # or "per-package" for Go
  module_size_limit_bytes: 262144       # skip huge generated files
  ollama_concurrency: 4
  post_ingest_reflect: true
  post_ingest_analyze: false            # set true to trigger cross-project analysis after each ingest

analyze:
  default_compression_ratio: 1.15       # relaxed vs scheduled reflection
  cross_project_min_projects: 2
  frame_bias: ["BugPattern", "RetryPattern", "ReliabilityPattern", "SecurityPattern", "Principle"]
  leiden_full_refresh: true
  schedule: "0 4 * * 0"                 # weekly Sunday 04:00

dedup:
  strategy: "psi_then_llm"
  llm_review_threshold: 0.85

log:
  path: "~/.cortex/log.jsonl"
  fsync: "per_transaction"

docker:
  compose_file: "~/.cortex/docker-compose.yaml"
```

---

## Data Migration from MemPalace

### Strategy

1. Python helper exports all MemPalace drawers (ChromaDB), entities and triples (SQLite KG), and diaries as a single JSONL export file — one JSON object per MemPalace record.
2. `cortex migrate --from-mempalace=<export-file>` maps each record to datoms and appends them to the log.
3. The normal write pipeline runs on each migrated record, deriving links and resolving subjects.
4. A final `cortex reflect --since=<migration-start>` pass consolidates the migrated episodes into semantic frames where patterns emerge.

### Mapping

| MemPalace concept | Cortex concept |
|---|---|
| Drawer | Episodic `Entry` with `kind=Observation` |
| Wing | Facet value (`facet/domain` or `facet/artifact` depending on name) |
| Room | Facet value (`facet/project` or `facet/artifact`) |
| Entity | `:Subject` with PSI derived from entity name |
| Triple | `REFERENCES` edge on a synthesized frame, or a `:Subject` attribute, depending on predicate |
| Diary entry | Episodic `Entry` with `kind=SessionReflection`, attached to a synthesized trail per session |
| AAAK format | Flattened to plain text |

### Link derivation

Migrated entries use the normal write-time link-derivation pipeline. The orphan problem from the v1 spec does not arise: every migrated entry is anchored to subjects via PSI and to trails via per-session synthesis, and A-MEM linking fills in the rest.

### Reporting

`cortex migrate` reports:
- N datoms appended.
- N entries created.
- N subjects created vs. reused.
- N trails synthesized.
- N derived links.
- N frames proposed by the post-migration reflection run.

---

## Agent Integration

System-prompt snippet for agents:

```markdown
## Cortex — Knowledge System

### Session lifecycle
At session start:
    export CORTEX_TRAIL_ID=$(cortex trail begin --agent=<your-name> --json | jq -r .id)

At session end (MANDATORY):
    cortex trail end

### During work
Record observations as you make them:
    cortex observe "..." --kind=Observation --facets=domain:...,project:...,... --json

When you discover a bug pattern, race, or decision, still use `observe` — do NOT
try to write a frame. The reflection job will promote recurring patterns into
frames automatically.

Never author links. Cortex has no `cortex link` command. Linking is the system's
job, not yours.

### Before acting
Search Cortex before answering questions about past context:
    cortex recall "query" --json
    cortex community show <id> --json     # for broader topic context

### Rules
- Always use --json for machine-readable output.
- Always write observations inside a trail.
- Never skip trail end — it is the consolidation anchor.
- Prefer PSI subjects when referring to libraries, CVEs, or ADRs.
```

---

## Testing Strategy

- **Datom log determinism.** Replaying a log into empty Weaviate + Neo4j produces byte-identical derived state across runs (up to embedding non-determinism, which is isolated and masked).
- **Merge correctness.** Two logs with overlapping transactions merge to the set union, deduplicated by `tx`. Conflict-free by construction.
- **Frame validation.** Writing a frame with missing required slots fails with a clear error.
- **Facet enforcement.** Writing an entry without required facets fails.
- **Write-time linking.** The linking step is tested against fixtures with known nearest neighbors and asserted link outputs.
- **PSI merge.** Two entries referring to `subject:lib/redis` unify under a single subject node, and a subsequent query surfaces both.
- **Reflection correctness.** Given a fixture of N similar episodic entries, the reflection job proposes the expected frame type, populates the required slots, and emits `DERIVED_FROM` edges.
- **Community detection.** Leiden over a known graph produces the expected hierarchical partitioning.
- **HippoRAG retrieval.** Seeded PPR on a known graph returns expected top-K for a known query; ACT-R rerank reorders by activation components correctly.
- **Forgetting.** After simulating N days with no retrievals, base activation decays according to the configured exponent; reinforcement restores it on retrieval.
- **Trail lifecycle.** A trail with three observations produces the expected trail summary and attaches all three as `IN_TRAIL` with correct order.
- **Concurrent writes.** Multiple goroutines writing simultaneously produce a consistent log and consistent derived indexes.
- **Migration.** MemPalace export → cortex import → verify count, subject resolution, facet assignment, and reflection pass.
- **Ingestion end-to-end.** Ingest a fixture repository; verify the expected number of module entries, pattern observations, decision hints, and PSI subject resolutions; verify a post-ingest trail exists with the correct membership; verify the post-ingest reflection pass ran and is scoped to the just-ingested project.
- **Incremental ingestion.** Re-ingest after a file change; verify only the changed module is reprocessed and deleted files produce retraction datoms (with the prior entries remaining in the log but absent from default retrieval).
- **Multi-language ingestion.** Ingest Go and Python fixture projects with different module strategies (`per-package` vs `per-file`); verify both land correctly and share PSI subjects where dependencies overlap.
- **Ingestion resume.** Kill an ingest mid-run; verify `cortex ingest resume` completes the missing modules without duplicating the ones already written.
- **Cross-project analysis.** Ingest two fixture repositories that share a library and implement a recurring pattern (e.g., HTTP retry with exponential backoff); run `cortex analyze --find-patterns`; verify a frame is produced with `cross_project=true` and `DERIVED_FROM` edges spanning both projects.
- **Project retraction.** `cortex project forget <name>` emits retraction datoms for all entries from that project; those entries drop out of default retrieval but remain recoverable via `cortex as-of` and `cortex history`.
- **CLI.** Each command tested in both human and `--json` modes; exit codes 0 success, 1 not found, 2 error.
- **Doctor.** `cortex doctor` detects all failure modes (Docker down, Weaviate unreachable, Neo4j auth wrong, Ollama missing, GDS plugin absent, frame schema invalid, Ollama concurrency insufficient for ingest).
- **Offline.** Docker containers up with no internet — all operations work, including ingestion and analysis.

---

## Success Criteria

Phase 1 is complete when:

1. `cortex` binary builds and runs on macOS ARM64.
2. `cortex up` starts Weaviate + Neo4j via Docker Compose with GDS installed.
3. The datom log is the source of truth; `cortex rebuild` reconstructs both backends from the log alone.
4. `cortex observe` writes are recorded as datoms, applied to both indexes, and derive typed links via the A-MEM pipeline.
5. `cortex trail begin` / `end` produces replayable session trails with LLM-generated summaries.
6. `cortex recall` returns HippoRAG PPR + ACT-R-ranked results; the `--mode` variants all function.
7. `cortex reflect` consolidates episodic entries into semantic frames that pass the MDL compression check.
8. Leiden community detection produces a hierarchical topic structure with LLM-written summaries at each level.
9. PSI subjects merge duplicate references across projects.
10. Base-level activation decays over time and reinforces on retrieval; stale entries fall out of default ranking without being deleted.
11. `cortex merge` combines two datom logs into a consistent state.
12. `cortex migrate --from-mempalace` ingests existing MemPalace data without loss.
13. `cortex history <id>` and `cortex as-of <tx>` demonstrate time-travel over the datom log.
14. Multiple agents can read and write concurrently without errors or lost writes.
15. `cortex doctor` detects and reports all failure modes.
16. Reads complete in <500 ms (concept extraction + PPR + rerank), writes in <1.5 s (including embedding + link derivation). These envelopes account for Ollama latency and are relaxed from the v1 targets, which did not account for on-the-read-path embedding.
17. `cortex ingest <path>` walks a real fixture project, writes episodic entries through the standard pipeline, resolves library dependencies as PSI subjects, attaches all writes to a synthesized ingest trail, and runs a scoped post-ingest reflection pass.
18. Incremental re-ingestion processes only changed modules and retracts entries for deleted files via datoms, without losing history.
19. `cortex analyze --find-patterns` over two ingested projects that share a library and a recurring pattern produces at least one frame marked `cross_project=true` with `DERIVED_FROM` edges spanning both projects.
20. **Value criterion (load-bearing).** After ingesting two real projects, `cortex recall "<cross-cutting concept>"` — for example, `"retry logic"` or `"error handling"` — returns observations from both projects in its top-N, **and** `cortex analyze --find-patterns` produces a corresponding frame whose exemplars include episodes from both projects. This is the end-to-end proof that Cortex delivers its primary value proposition. No amount of passing unit tests substitutes for it; Phase 1 is not complete until this criterion is met on a real (non-fixture) pair of projects from the user's own work.
21. All tests pass.

---

## Future: Phase 2 — Visualization

Interactive visualization of entries, frames, trails, communities, and subjects. This is the only capability deliberately deferred from Phase 1.

Phase 1 ships the complete memory substrate, the complete retrieval pipeline, the complete ingestion pipeline, and cross-project analysis — because none of these in isolation are sufficient to demonstrate Cortex's primary value proposition, and a system that cannot demonstrate its value proposition is not shippable. Visualization is a true increment on top of a system that already works end-to-end; deferring it is safe, because the substrate, schema, and CLI are all complete without it.

Technology TBD (likely a local web UI over Neo4j's native rendering, or a D3 / Cytoscape.js layer over the Neo4j Bolt driver).
