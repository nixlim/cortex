# Cortex — Architecture (v2)

Supersedes `architecture-vision.md`.
Grounded in `research-report.md`.

## Problem Statement

MemPalace works but has fundamental limitations that prevent it from becoming a shared knowledge substrate for multi-agent workflows. The goal is to build a system that:

- Serves as persistent memory for multiple concurrent agents.
- Enables cross-project pattern discovery across all codebases.
- Stores knowledge you can't grep for — understanding, relationships, decisions, patterns.
- Runs as a single Go CLI binary backed by Weaviate + Neo4j in Docker.
- Scales from personal knowledge to multi-agent collaborative intelligence.
- **Consolidates** fine-grained episodes into reusable patterns over time.
- **Forgets** what stops being useful without losing what was true.

## Core Insight

Earlier drafts used Zettelkasten as the organizing metaphor. Zettelkasten is a *human thinking ritual*: it exists because human working memory holds ~7 items and needs linking-as-thinking to offload cognition onto paper. Language models have 200k–1M token contexts, vector similarity on tap, and no need for mnemonic scaffolding. Transplanting Zettelkasten to agents produces two failures in practice: (1) mandatory authored linking degenerates into a hairball of low-signal `relates_to` edges when the linker is an agent under duress, and (2) the model has no theory of salience, decay, or consolidation, which is exactly what a long-lived cross-project store needs most.

Cortex replaces the Zettelkasten metaphor with a stack drawn from cognitive science and modern agent-memory research: **Complementary Learning Systems** for the episodic/semantic split, **CoALA** for the top-level partition, **HippoRAG** for retrieval, **Memex trails** for session-as-entity, **Minsky frames** for structured notes, **Ranganathan facets** for classification, **GraphRAG** for derived topic structure, and **Datomic-style accretion** for the storage substrate. Linking becomes a *derived property of the substrate* rather than an authoring obligation.

See `research-report.md` for the full justification and citations.

---

## Design Principles

### 1. CLI-only, no MCP

MCP adds transport complexity (JSON-RPC over stdio) for a problem that does not exist. Claude Code and every other modern agent framework already support running CLI commands. The `bd` (beads) tool proves this pattern works — agents call CLI commands, parse output, move on.

Benefits: no server process to manage, no connection state, no zombie processes; trivially debuggable (run the same command the agent ran); works in any shell, cron job, git hook, or framework; composable with unix tools; no SDK dependency on the client side.

### 2. Store observations, patterns, decisions — not code

Raw code lives in git. Structure lives in code-intelligence tools (GitNexus, LSP). Cortex stores what emerges from *thinking about* code: observations, recurring patterns, decisions and their rationales, architectural principles, and agent insights.

### 3. Two memory systems, not one

Mammalian brains evolved two memory systems because you cannot have both fast one-shot learning and interference-free generalization in a single substrate (McClelland, McNaughton & O'Reilly, 1995). Cortex follows suit:

- **Episodic memory**: fast, append-only, high-fidelity, indexed by time. Records what happened. Every observation, session event, and agent insight lands here first.
- **Semantic memory**: slow, curated, abstracted. Records what is *true across episodes*. Written only by a deliberate consolidation process, not by agents directly during work.

A third partition — **procedural memory** — holds verified reusable patterns (retry strategies, architectural decision records, refactoring playbooks). This is what software-engineering agents actually draw from most in practice, and it is the primary deliverable of the system for multi-project work. CoALA (Sumers et al., 2023) formalizes this three-way partition for language agents.

### 4. Datoms as the storage atom

Everything in Cortex — observations, notes, links, facet assignments, community memberships, activation values — is an immutable `(Entity, Attribute, Value, Transaction, Agent, Confidence)` datom. Notes, sessions, and trails are *views* over datoms.

Consequences:
- **Event log and storage are the same thing.** The datom log is the source of truth; Weaviate and Neo4j are derived indexes.
- **Merge** is set union of datoms, deduplicated by transaction ID.
- **Time travel** is querying as of a transaction ID.
- **Replay** reconstructs both derived indexes from the datom log alone.
- **Retractions are themselves facts.** Nothing is ever truly deleted; everything is accretive.

This eliminates the need for a separate JSONL event log as proposed in the v1 spec. The datom log *is* the event log.

### 5. Notes are frames, not free text

Every semantic and procedural entry is a **typed Minsky frame** with named slots: `BugPattern`, `DesignDecision`, `RetryPattern`, `LibraryBehavior`, `ObservedRace`, `Principle`, and so on. Each slot value is a **Wikidata-style claim** with qualifiers (time, project, agent, confidence) and references (the episodic datoms that supported it).

Why: language models are excellent at filling structured slots given a frame, and unreliable at deciding the shape of a note from scratch. Frames also make entries machine-comparable — two `BugPattern` frames can be diffed slot-by-slot across projects, which is precisely the cross-project discovery operation Cortex is built for.

Episodic entries are lighter-weight: type, body text, facet metadata, provenance. Episodic writes must be cheap.

### 6. Faceted classification, not folders or hand-assigned topics

Every entry carries mandatory faceted metadata — roughly:

- **Kind** (Observation, Pattern, Decision, Principle, Trace, Summary, ...)
- **Domain** (Reliability, Security, Performance, UX, ...)
- **Artifact** (Service, Library, Module, Schema, Infrastructure, ...)
- **Project** (canonical project identifier)
- **Time** (valid-time — when the observation was made / decision taken)
- **Language** (programming language, when applicable)

Ranganathan's insight (1933): instead of placing a document in one hierarchical bucket, synthesize its address from orthogonal facets. Faceted queries handle the bulk of what Cortex v1 called "topics"; embeddings and graph communities handle the rest.

### 7. Linking is derived, not authored

Agents do not author links. The CLI has **no `link` verb**. Three mechanisms create edges:

1. **Write-time LLM derivation (A-MEM style).** On every write, an LLM proposes typed links to similar existing notes and may update the context descriptions of neighboring notes. This is the only time an LLM sees the new note in context with its nearest neighbors.
2. **Topic Maps PSI merge.** Canonical subject identifiers for recurring entities (CVEs, libraries, known bug classes, ADRs). Two notes sharing a PSI refer to the same subject and their claims unify under a formal, predictable merge operation. Replaces ad-hoc similarity-threshold deduplication.
3. **Hierarchical community detection (GraphRAG style).** Periodic Leiden clustering over the semantic graph produces multi-level communities. An LLM writes a summary for each community at each level. Communities are first-class entities with their own stable IDs and are Cortex's authoritative "topics."

The substrate does the linking. The agent does the thinking.

### 8. Trails as first-class entities

Bush's Memex (1945) argued the distinctive value of a knowledge system is not individual records but the *trails* — named, ordered, replayable sequences capturing how a researcher arrived at an insight. An AI coding session *is* a trail: a sequence of observations, hypotheses, code reads, decisions, fixes, outcomes.

Cortex records every session as a named trail. Trails attach to the semantic and procedural entries they produced. Cross-project pattern discovery becomes "find trails whose shape resembles this one," which is a far stronger signal than `relates_to` edges.

### 9. Retrieval = HippoRAG PPR + ACT-R activation

The default retrieval path (from Gutierrez et al., NeurIPS 2024, plus Anderson's ACT-R activation equation):

1. **Concept extraction.** An LLM extracts concept mentions from the agent's query.
2. **Seed PPR.** Personalized PageRank over the Neo4j graph, seeded from the extracted concepts.
3. **Rerank by activation.** For each candidate, compute ACT-R-style activation:
   `activation = base_level(recency, frequency) + spreading(from_seeds) + similarity(embedding, query) + importance(LLM_scored)`
4. **Return** the top-N entries along with their trails and the community summaries they belong to.

One operation delivers both intentional traversal (via graph structure) and serendipitous discovery (via embeddings), with a principled ranking function that accounts for recency and use.

### 10. Forgetting is a feature

Base-level activation decays by a power law. Successful retrievals reinforce. Stale entries are not deleted — accretion is preserved — but they fall out of default retrieval rankings. This is the one thing Zettelkasten fundamentally cannot express, and without it any long-lived multi-project store becomes a swamp.

---

## Dual-Backend Architecture

The Weaviate + Neo4j split from v1 is preserved, but their roles are now sharper:

- **Datom log** (local append-only file) — source of truth. One JSON object per transaction. Independent of either database. Portable, mergeable, replayable.
- **Weaviate** — vector embeddings, semantic search, hybrid BM25+vector, Ollama integration for embeddings. Stores episodic entries (for fast similarity recall) and semantic/procedural frames (for concept-level retrieval). Derived from the datom log.
- **Neo4j** — graph storage, Cypher queries, multi-hop traversal, Personalized PageRank, Leiden community detection via the Graph Data Science plugin. Stores the structural view: entries, derived links, trails, community hierarchies, facet values. Derived from the datom log.

```
┌───────────────────────────────────────────────────────────┐
│                    cortex CLI (Go)                        │
│                                                           │
│  cortex observe "..." --kind=... --facets=...             │
│  cortex recall "query"                                    │
│  cortex reflect                                           │
│  cortex trail begin|end|show                              │
│  cortex pin <id> / cortex evict <id>                      │
│  cortex merge <datom-log>                                 │
│  cortex community show <id> [--depth=N]                   │
└───────────────┬───────────────────────────────────────────┘
                │
     ┌──────────▼────────────┐
     │     Datom Log         │   ← source of truth
     │  ~/.cortex/log.jsonl  │
     └──────┬────────┬───────┘
            │        │
      ┌─────▼──┐ ┌───▼─────┐
      │Weaviate│ │  Neo4j  │   ← derived indexes
      │        │ │         │
      │ vectors│ │ graph   │
      │ hybrid │ │ PPR     │
      │ search │ │ Leiden  │
      └────┬───┘ └─────────┘
           │
      ┌────▼────┐
      │ Ollama  │
      │         │
      │ embed   │
      │ summar. │
      │ extract │
      │ reflect │
      └─────────┘
```

---

## Sources of Knowledge

Cortex ingests knowledge from four sources and transforms it via two substrate-level processes. Agents and humans write only to episodic memory; semantic memory is populated exclusively by system transforms.

### Sources (write paths)

**1. Agent observations during work.** Agents record episodic entries as they work:
```
cortex observe "API design has TOCTOU race: token validation and resource access are not atomic."
  --kind=ObservedRace
  --facets=domain:Security,artifact:Service,project:payment-gateway,language:go
  --trail=$CORTEX_TRAIL_ID
```
No link is authored. The substrate derives links at write time.

**2. Human input.** Humans write episodic observations and decision records through the same CLI. The intake is identical to the agent path — the distinction is recorded only in the `agent` attribute on each datom.

**3. Code ingestion.** `cortex ingest <project-path>` walks a codebase, generates structured summaries and pattern observations per module via Ollama, and writes them through the standard write pipeline. Facets, trails, PSI resolution, and write-time link derivation all apply. Re-ingestion is incremental: each project records its last-ingested commit SHA; only modules touched by changed files are reprocessed, and deleted files produce retraction datoms. This is the highest-volume write path and the main driver of cross-project signal — libraries seen across multiple projects become PSI hubs, and recurring code patterns become candidates for consolidation.

**4. MemPalace migration.** One-time import of existing MemPalace data. Drawers map to episodic entries; session history synthesizes into trails; wings and rooms become facet values; entities become PSI subjects. Migrated content goes through the normal write pipeline and participates in reflection alongside native data.

### Transforms (system-internal)

**Reflection / consolidation.** Scheduled job that reads recent episodic memory, clusters by concept and facet, and proposes semantic frames when clusters pass an MDL-flavored compression check. The only path into the semantic store — agents cannot write frames directly. This is the analogue of hippocampal replay consolidating episodes into neocortex.

**Cross-project analysis.** `cortex analyze --find-patterns` runs an unrestricted consolidation pass across multiple projects with no time filter, designed to surface patterns only visible in aggregate. Same pipeline as scheduled reflection, with cross-project candidate selection (clusters must draw from ≥2 projects) and a relaxed compression threshold. Produces frames with `DERIVED_FROM` edges spanning projects; these frames are marked `cross_project=true` and carry higher importance weight in retrieval ranking. Also triggers a full Leiden re-run and community summary refresh.

---

## Queries Cortex Should Answer

Same ambition as v1, now answered by concrete mechanisms:

- *"How have I implemented retry logic across projects?"* → faceted query on `Kind=RetryPattern` + community summary for the reliability cluster.
- *"What design decisions led to the auth middleware in project X?"* → trail lookup for the session that produced the decision frames, reranked by activation.
- *"Which projects use Redis, and why?"* → PSI lookup on the Redis subject identifier + qualifier filter on `purpose`.
- *"Show me all race conditions discovered across projects."* → faceted query on `Kind=ObservedRace`, grouped by community.
- *"What did agent X learn about this codebase last week?"* → trail history filtered by agent and time.
- *"Shortest conceptual path between project A's auth and project B's auth?"* → PPR seeded from both project's auth frames + Neo4j shortest path.
- *"What topics have emerged this month?"* → new communities in the latest Leiden run vs. previous run.
- *"Starting from 'error handling', show me the chain of related observations."* → entry-point lookup on the error-handling community summary + trail walk.

---

## Delivery Phases

### Phase 1: Full system

Phase 1 ships the complete system. The only capability deliberately deferred is visualization. Everything else — substrate, write paths, retrieval, consolidation, ingestion, and cross-project analysis — lands together, because Cortex cannot demonstrate its primary value proposition (cross-project pattern discovery) without all of them.

- Go CLI binary. Datom log as source of truth. Weaviate + Neo4j as derived indexes running in Docker Compose.
- CoALA three-store partition (episodic / semantic / procedural).
- Minsky frames for semantic and procedural entries; Ranganathan faceted classification.
- Memex trails as first-class session entities.
- Write-time derived linking (A-MEM style); Topic-Maps PSI merge; no authored links.
- HippoRAG Personalized PageRank + ACT-R activation retrieval.
- Scheduled reflection / consolidation with an MDL-flavored compression check.
- Hierarchical Leiden community detection with LLM-written summaries at each level.
- Forgetting via ACT-R base-level decay and reinforcement on retrieval.
- **Code ingestion pipeline** (`cortex ingest <project-path>`) — walks a codebase, summarizes modules and detects patterns via Ollama, writes episodic entries through the standard write pipeline, resolves libraries and recurring entities as PSI subjects, runs a targeted post-ingest reflection pass, and supports incremental re-ingestion against a stored commit SHA.
- **Cross-project analysis** (`cortex analyze --find-patterns`) — unrestricted consolidation across multiple projects to surface patterns only visible in aggregate.
- MemPalace migration.
- Diary subsumed into episodic entries via the `SessionReflection` frame.

Phase 1 is not complete on capability checks alone. It is complete when at least one cross-project query — for example, `cortex recall "retry logic"` over two ingested projects — returns observations from both projects in its top-N, **and** `cortex analyze --find-patterns` produces a corresponding frame whose `DERIVED_FROM` edges include exemplars from both projects. This is the end-to-end proof of value and is itself a success criterion in the spec.

### Phase 2: Visualization

Interactive visualization of entries, frames, trails, communities, and subjects. The only capability deferred from Phase 1. Technology TBD (likely a local web UI over Neo4j's native rendering or a D3 / Cytoscape.js layer).

---

## What Has Been Removed from v1

- **Mandatory authored linking.** Replaced by write-time derivation + PSI merge + community structure.
- **`relates_to` as a catch-all edge.** Forbidden. Edges are either typed frame-slot references, PSI merges, or community-membership edges.
- **Zettelkasten sequences.** Replaced by Memex trails, which are strictly more expressive (named, ordered, replayable, attachable to entries).
- **"Topics via graph community detection" alone.** Replaced by facets plus GraphRAG hierarchical community summaries. Topics have names and summaries, not just node sets.
- **Notes as the storage atom.** Datoms are the atom. Notes and trails are views.
- **The separate JSONL event log.** The datom log subsumes it.
- **Fixed similarity-threshold deduplication.** Replaced by PSI-based identity merge where an identifier is available, and write-time LLM judgement otherwise.
- **The `cortex link` verb.** Removed.

## What Has Been Kept from v1

- The CLI-only, no-MCP stance.
- The Weaviate + Neo4j dual backend.
- The "store understanding, not code" scope.
- Ollama as the local embedding and LLM provider.
- Docker Compose infrastructure footprint.
- The atomicity principle (one idea per claim) — now enforced by frame slots instead of discipline.
- The commitment to emergent structure over top-down taxonomy — now served by facets + derived communities instead of graph-topology-alone.
