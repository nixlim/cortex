# Cortex — Research Report: Knowledge Organization Models for an AI-First Memory Substrate

Date: 2026-04-09
Purpose: Evaluate memory and knowledge organization techniques from information theory, library science, cognitive science, hypertext history, and modern AI agent research, and identify those most suitable for Cortex — a persistent, multi-agent, cross-project knowledge store.
Scope: Survey → assessment → recommended organizing model. Intended to inform a replacement of the current Zettelkasten-based architecture draft.

---

## 1. Motivation

The current Cortex architecture draft (`architecture-vision.md`, `phase-1-spec.md`) uses **Zettelkasten** as its organizing metaphor: atomic notes, mandatory typed links authored by the writer, sequences, no folder taxonomy, topics computed via graph community detection. The Memory Palace (Method of Loci) was implicitly referenced via the MemPalace predecessor.

Both are **human memory techniques**. Zettelkasten exists because human working memory holds ~7 items and needs linking-as-thinking to offload cognition onto paper. The Method of Loci exists as a pure mnemonic scaffold for recall from unreliable biological storage. Language models have 200k–1M token contexts, vector similarity on tap, and no need for mnemonic ritual. The forcing functions that make these techniques productive for humans become tax and noise when transplanted to agents.

Specific failure modes of the current draft:

- **Mandatory authored linking** produces a hairball of low-signal `relates_to` edges when agents are forced to link under duress. Humans link because the act of linking *is* the thinking; agents link because the CLI demanded it.
- **No theory of salience or forgetting.** Zettelkasten has no way to express that knowledge decays, is reinforced by use, or should be consolidated from fine-grained episodes into abstracted patterns.
- **No distinction between event and fact.** A session observation ("agent X saw a TOCTOU race today in project Y") and a consolidated principle ("this class of race appears in all projects touching shared-mutable auth state") are the same kind of object. They shouldn't be.
- **Topics as post-hoc community detection only.** Louvain-ish clustering on a few thousand weakly-linked nodes produces labels that look meaningful and aren't. There's no mechanism to *write down what a cluster means* at multiple resolutions.
- **Linking as authoring obligation, not derived property.** The substrate is doing no work; the agent is doing all of it.

The framing question: what techniques from information theory, library science, cognitive science, hypertext history, and modern AI memory research are better matched to the stated goals?

Goals (unchanged from the current draft, restated here for reference):

1. Persistent memory shared across multiple concurrent AI agents.
2. Cross-project pattern discovery ("where else have I seen this race condition / retry pattern / design decision?").
3. Store *understanding* — observations, patterns, decisions, principles, agent insights. Not raw code (git has that). Not code structure (LSP/code-graph tools have that).
4. Support both intentional traversal (graph walks) and serendipitous discovery (semantic similarity).
5. Machine-first: agents are the primary read **and** write users.
6. Atomic, mergeable, replayable (event-sourced).

---

## 2. Survey by Domain

### 2.1 Information & Library Science

**Ranganathan's Faceted Classification / PMEST** (Colon Classification, 1933).
Decomposes any subject into orthogonal facets — Personality, Matter, Energy, Space, Time. Instead of placing a document in one hierarchical bucket, you *synthesize* its address from facets. What it optimizes for: multi-dimensional retrieval without pre-committing to a taxonomy tree.

**Fit to Cortex: very high.** A "retry pattern observed in payment service on 2026-04-01" is not a node in a tree; it is `(Kind=Pattern, Domain=Reliability, Artifact=Service, Project=Foo, Time=2026-04)`. Faceted metadata serves cross-project discovery almost for free: "show me all Patterns with Domain=Reliability across all Projects" is a faceted query. Crucially, facets *compose*: you don't have to pick one axis.

**Steal:** faceted metadata as the mandatory shape for every note. Drop the idea of "topics" as a property of the graph alone.

**Topic Maps (ISO/IEC 13250-2:2006).**
Separates *subjects in the world* from *topics in the map* and defines a formal merge operation via **Published Subject Identifiers (PSIs)**: two topics sharing a PSI are the same thing, and their properties unify. The standard spells out exactly how merges propagate.

**Fit to Cortex: extremely high.** Multi-agent writes about the same library, CVE, bug class, or architectural decision will collide. PSI-based merge is the principled reconciliation strategy. It generalizes the ad-hoc `similarity > 0.95 = dedup` the current draft proposes into a typed, predictable operation.

**Steal:** canonical subject identifiers for recurring entities, and a formally defined merge operation grounded in identifier equality rather than embedding similarity.

**SKOS (W3C Simple Knowledge Organization System).**
Thesaurus vocabulary: `broader`, `narrower`, `related`, `broaderTransitive`. Notably, `broader` is **deliberately non-transitive by default** because real-world hierarchies aren't.

**Fit: moderate.** Gives Cortex a standards-based vocabulary instead of inventing edge types. Also provides a natural export path.

**Steal:** SKOS-style edge semantics and the explicit-transitivity discipline.

**Citation Indexing (Garfield, 1955 onward).**
Treats citation itself as the primary organizing signal: who cites whom, clustered into co-citation communities. The direct ancestor of PageRank and of modern graph-community techniques. Critical design observation: citations are **typed by convention**, not by mandate.

**Fit: lesson, not model.** Confirms that high-quality link structure can emerge without anyone being forced to declare link types. Supports the thesis that mandatory authored linking is the wrong forcing function.

---

### 2.2 Cognitive Science and AI Memory Models

**Minsky Frames (1974) and Schank/Abelson Scripts.**
Frames are stereotyped situations with named slots and defaults. Unlike raw semantic networks, frames allow inheritance and exceptions. Scripts extend frames to sequences of stereotyped events.

**Fit: high as a note schema.** A `BugPattern` frame, a `DesignDecision` frame, an `ObservedRace` frame — each with required and optional slots — replaces "free-form atomic notes." Language models are excellent at filling structured slots when given a frame, and unreliable at deciding the shape of a note from scratch. Frames also make notes machine-comparable: two `BugPattern` frames can be compared slot-by-slot.

**Steal:** typed note frames with slots as the semantic-store write format.

**ACT-R Declarative Memory (Anderson).**
ACT-R retrieves "chunks" via an **activation equation**:

> activation = base-level (recency + frequency, power-law decay) + spreading activation from current context + similarity to cue

ACT-R has no explicit "links." Linkage is implicit in co-occurrence. Activation propagates at retrieval time. This is the most rigorous cognitive-science answer to "how should a memory system rank candidates?"

**Fit: very high.** The activation equation is directly implementable on Weaviate + Neo4j: base-level = recency/frequency decay stored on nodes; spreading activation = Personalized PageRank from the query seed; similarity = vector distance. This fixes the current draft's total absence of a theory of salience or forgetting.

**Steal:** the ACT-R activation equation as the default ranking function for retrieval, including explicit temporal decay.

**Complementary Learning Systems — McClelland, McNaughton & O'Reilly (1995).**
Mammalian brains evolved **two memory systems** for a reason. The **hippocampus** does sparse, pattern-separated, one-shot encoding of specific episodes. The **neocortex** does slow, overlapping, statistical extraction of regularities. Periodic **replay** consolidates episodic memories into cortical structure. The argument (formalized mathematically) is that you *cannot* have both fast one-shot learning and catastrophic-interference-free generalization in a single network.

**Fit: this is the architectural blueprint.** It directly implies Cortex should split:

- **Episodic store** — raw observations, session events, agent insights. High fidelity, time-indexed, written fast and cheap, never edited.
- **Semantic store** — consolidated patterns, principles, decisions. Written only by a deliberate **consolidation job** that reads episodic memory and abstracts regularities.

The 2025 agent-memory literature has converged on exactly this split.

**Steal:** the two-tier (plus procedural, below) architecture as the top-level storage decision.

**CoALA — Cognitive Architectures for Language Agents (Sumers et al., 2023, arXiv:2309.02427).**
Imports the cognitive-science memory taxonomy directly into language-agent architecture: working memory + long-term memory partitioned into **episodic** (events), **semantic** (facts), and **procedural** (skills). Recent surveys show software-engineering agents lean heavily on **procedural** memory — verified patterns, ADRs, reusable playbooks — which is exactly Cortex's target.

**Fit: very high.** The three-way partition gives Cortex a principled top-level ontology with natural query planning (which store to hit) and natural write rules (which store accepts which kind of entry from which actor).

**Steal:** the episodic/semantic/procedural partition as Cortex's top-level schema.

**Spreading Activation and Semantic Networks (Quillian, Collins & Loftus).**
The foundational model of associative retrieval in cognitive psychology. A concept is activated; activation spreads along edges with decay; items above a threshold are retrieved. Spreading activation is the theoretical grandparent of Personalized PageRank and of graph-based retrieval in RAG systems.

**Fit: foundational.** Provides the theoretical grounding for PPR-based retrieval. Not a design input on its own.

---

### 2.3 Hypertext Prehistory

**Vannevar Bush, Memex, and *trails* ("As We May Think", Atlantic, 1945).**
Bush's central object in the Memex was **not the link** — it was the **trail**: a named, ordered, replayable sequence of items a researcher followed to reach an insight. Trails are sharable, annotable, and outlive the session that produced them. Bush's claim was that the distinctive value of a knowledge system is not its individual records but the paths between them that capture reasoning.

**Fit: unreasonably high, and missing from the current draft.** An AI coding session *is a trail*: a sequence of observations, hypotheses, code reads, decisions, fixes. Storing sessions as first-class, replayable trails serves both "shared memory across agents" and "cross-project pattern discovery" far better than atomic notes connected by `relates_to` edges. Cross-project pattern discovery can literally mean "find trails whose shape resembles this one."

**Steal:** trails as a first-class entity type, not a derived sequence of links.

**Ted Nelson, Project Xanadu — transclusion and bidirectional links.**
Transclusion = embedding a live excerpt from one document in another, so updates propagate and the source remains visibly connected. Bidirectionality = every link is navigable from both ends.

**Fit: high.** Transclusion solves the case where multiple notes reference the same observation: reference once, transclude rather than copy. Bidirectionality is free in a graph database but matters as a data-model commitment — no dangling back-references.

**Steal:** transclusion for quoted fragments; bidirectionality as a substrate invariant.

**Doug Engelbart, NLS/Augment.**
Structured documents with fine-grained addressing; the premise that tools should augment human intellect rather than replace it. More a philosophy than a mechanism. Worth noting because it positions the knowledge system as a collaboration partner, not an oracle.

---

### 2.4 Modern Agent Memory Architectures

**MemGPT / Letta (Packer et al., arXiv:2310.08560).**
Two-tier memory modeled on operating-system virtual memory: in-context "core" memory (analogous to main memory) and out-of-context archival (analogous to disk), with the agent **self-managing paging** via explicit function calls.

**Fit: the operational model for Cortex's read side.** Cortex is the archival tier; the agent's context window is core; retrieval is paging. The explicit paging interface (pin, evict, swap, search) is better than a pure query-and-hope model.

**Steal:** the self-managed paging interface for the CLI surface.

**Microsoft GraphRAG (Edge et al., arXiv:2404.16130).**
Extracts an entity graph from a corpus; runs **hierarchical Leiden community detection**; then uses an LLM to **pre-generate a summary for each community at each level of the hierarchy**. Global queries are answered by fanning out over community summaries and map-reducing.

**Fit: directly applicable.** This replaces "topics via graph community detection" (current draft) with "topics *and their precomputed summaries at multiple resolutions*," which is the whole point. An agent asking "what do I know about reliability patterns across projects?" gets a precomputed multi-level answer rather than a walk of the graph.

**Steal:** hierarchical Leiden clustering plus LLM-generated community summaries as a derived, refreshable index over the semantic store.

**HippoRAG (Gutierrez et al., NeurIPS 2024, arXiv:2405.14831).**
Explicitly models Complementary Learning Systems: a schemaless knowledge graph acts as the hippocampal index; the LLM plays the parahippocampal role for concept extraction from queries; **Personalized PageRank** seeded by query concepts performs single-hop and multi-hop retrieval 10–30× cheaper than iterative retrieval and up to 20% better on multi-hop QA benchmarks.

**Fit: this is the closest existing system to what Cortex should be.** Same stack shape as Weaviate + Neo4j. Same goals. Proven retrieval algorithm.

**Steal:** PPR over the graph, seeded by concepts extracted from the agent's current query, as the default retrieval algorithm. One operation, both intentional traversal and serendipity.

**A-MEM (Xu et al., NeurIPS 2025, arXiv:2502.12110).**
Notably uses Zettelkasten explicitly as its metaphor — but its novelty is that links are **LLM-generated at write time** from embedding neighborhood, and existing notes are **updated as new notes arrive** (memory evolution).

**Fit: cautionary — in the useful sense.** A-MEM is a proof that Zettelkasten-flavored structure can work for agents, but *only* when the linking is derived, not authored. This localizes Cortex's problem precisely: the current draft's weakness is not Zettelkasten per se; it is **mandatory authored linking**. Move linking to a background derivation process and Zettelkasten elements become tractable.

**Steal:** LLM-derived linking on write, with "memory evolution" updates to neighboring notes.

**Generative Agents (Park et al., 2023, arXiv:2304.03442).**
Append-only memory stream of observations. Retrieval ranks by weighted sum of **recency, importance (LLM-scored), and relevance (embedding similarity)**. Periodic **reflection** clusters recent observations and synthesizes higher-level abstractions, which become new memory items. Ablation studies showed reflection was load-bearing for coherent agent behavior.

**Fit: very high.** Reflection is the write-side mechanism for episodic-to-semantic consolidation that CLS predicts is necessary. The (recency, importance, relevance) triple is a simpler cousin of the ACT-R activation equation and is field-proven.

**Steal:** append-only episodic memory stream + scheduled reflection jobs + the (recency, importance, relevance) scoring triple.

---

### 2.5 Information Theory and Content-Addressable Memory

**Kanerva's Sparse Distributed Memory (1988).**
High-dimensional binary addresses. Writes store to all addresses within a Hamming radius of the address; reads read from all addresses near the cue and majority-vote. Effectively a mathematical ancestor of modern vector databases: associative, noise-tolerant, graceful under partial cues.

**Fit: theoretical grounding.** Justifies using Weaviate as a first-class store, not merely an index. The graceful-degradation-under-partial-cues property is inherent to the substrate. No further borrowing required — you already have this in Weaviate.

**Minimum Description Length (Rissanen).**
Principled answer to "what is worth remembering": a new observation is worth storing iff it shortens the description of future observations. In practice this is a proxy for "compresses future episodic content into a reusable semantic pattern."

**Fit: the scoring function for consolidation.** Gives the reflection job an objective: promote episodic observations to semantic patterns when they demonstrably compress future episodes.

**Steal:** MDL-flavored compression gain as the consolidation criterion.

**Hopfield Networks.**
Classic content-addressable associative memory with basins of attraction. Theoretically interesting, operationally subsumed by modern vector retrieval. No direct borrowing.

---

### 2.6 Other Practical Systems

**Datomic's Accretion Model.**
Immutable datoms `(Entity, Attribute, Value, Transaction, Op)`, bitemporal (valid-time + transaction-time), no in-place updates, retractions are themselves facts. Queries run over any point in history.

**Fit: directly.** This is the event-sourced substrate Cortex's goals already demand, with a mature, proven design. It eliminates the need to invent an event-log format separately from a storage format: **the datom is the event**.

**Steal:** the datom as Cortex's storage atom. Notes, links, facet assignments, and community memberships are all datoms. Notes are *views* over datoms, not a separate mutable kind of object.

**Wikidata Claim-with-Qualifier Model.**
Every statement is `(subject, property, value)` plus **qualifiers** (temporal, contextual, scope) plus **references** (provenance). A bare triple cannot express "agent X observed this in session Y under commit Z with confidence 0.7." A claim with qualifiers can.

**Fit: high.** Maps naturally onto datoms extended with agent, time, confidence, and source-session qualifiers.

**Steal:** the statement/qualifier/reference shape for atomic facts.

**Roam / Logseq block model.**
Every block has a stable ID; blocks are transcludable; queries run over blocks. The granularity is right: block-not-page is the correct unit for agents too. No novel mechanism, but a confirmation that block-level addressing works in practice.

**Spaced Repetition / FSRS decay curves.**
Useless as a study tool in this context, relevant as a **forgetting model**. Cortex needs forgetting: stale patterns should decay, reinforced ones should strengthen. Power-law decay plus reinforcement-on-retrieval gives a defensible schedule and is mathematically consistent with ACT-R base-level activation.

**Steal:** decay and reinforcement dynamics as first-class operations on memory items.

---

## 3. Synthesis: What Cortex Should Be

### 3.1 Diagnosis

The current draft chose a **human thinking method** as its organizing metaphor when four decades of cognitive-science-informed agent-memory research had already converged on a better-suited architecture. Zettelkasten — even steel-manned — loses on every stated goal:

| Goal | Zettelkasten | Better-fitting model |
|---|---|---|
| Shared memory across agents | Merge via informal link conventions | Topic-Maps PSI merge + Datomic accretion |
| Cross-project pattern discovery | Hope graph clusters cohere | GraphRAG hierarchical community summaries + PPR |
| Store understanding | Free-form atomic notes | CoALA episodic/semantic/procedural partition + Minsky frames |
| Intentional + serendipitous retrieval | Separate graph walk + embedding search | HippoRAG PPR seeded by extracted query concepts |
| Machine-first writes | Mandatory authored linking | A-MEM derived linking + Park-style reflection |
| Atomic / mergeable / replayable | Append-only notes | Datomic datoms as the substrate |
| Salience / forgetting | **Not expressible** | ACT-R activation + reinforcement decay |

The current draft's deepest failure is the absence of any theory of **what knowledge is worth**. A knowledge system without decay, reinforcement, or consolidation is a garbage-collected heap without garbage collection: eventually, signal drowns in accumulation.

### 3.2 Recommended Model: The Cortex Stack

**Storage substrate: Datomic-style accretive datoms.** Everything is an immutable `(Entity, Attribute, Value, Transaction, Agent, Confidence)` datom. Notes, sessions, trails, links, facet assignments, community memberships, and activation values are all datoms. Notes and sessions are *views* over datoms. This gives event-sourcing, merge, replay, and time-travel as substrate primitives.

**Top-level ontology: CoALA three-store partition.**

- **Episodic store.** Append-only memory stream of raw observations and session events. Written cheap and fast by agents during work. Never edited. Indexed by time, actor, source session, and embedding.
- **Semantic store.** Consolidated patterns, principles, decisions. Written only by a scheduled reflection / consolidation process that reads episodic memory and abstracts. Every semantic entry is a typed Minsky frame with named slots.
- **Procedural store.** Verified code patterns, retry playbooks, architectural decision records, reusable approaches. This is the store software-engineering agents draw from most heavily and is the system's primary deliverable for multi-project work.

**Note shape: Minsky frames with Wikidata-style claims.** Each semantic or procedural entry is a typed frame (`BugPattern`, `DesignDecision`, `RetryPattern`, `LibraryBehavior`, `ObservedRace`, `Principle`, ...) with required and optional slots. Each slot value is a claim with qualifiers (time, project, agent, confidence) and references (the episodic datoms that support it). No free-form semantic notes.

**Classification: Ranganathan facets.** Every note carries mandatory faceted metadata — roughly `(Kind, Domain, Artifact, Project, Time, Language)`. Faceted queries do most of the "topic" work; embeddings do the rest. No folder hierarchy. No `topic` field.

**Linking: derived, never authored under duress.** Three mechanisms:

1. **A-MEM-style write-time derivation.** On write, an LLM proposes links to similar existing notes and may update the context descriptions of neighboring notes.
2. **Topic Maps PSI merge.** Canonical subject identifiers for recurring entities (CVEs, libraries, bug classes, ADRs) so duplicates auto-merge under a formal, predictable operation.
3. **GraphRAG hierarchical community summaries.** Periodic Leiden clustering over the semantic graph, with LLM-written summaries at each level, stored as first-class community entities. Communities are the "topics" of the system.

Agents do not author links. The CLI has no `link` verb.

**Trails as first-class entities (Memex).** A coding session produces a named, ordered, replayable trail of episodic observations plus decision points plus outcomes. Trails attach to the semantic and procedural entries they produced. Cross-project pattern discovery becomes "find trails whose shape resembles this one" — a strictly more informative signal than `relates_to`.

**Retrieval: HippoRAG PPR plus ACT-R activation.** The default retrieval path:

1. Extract concepts from the agent's query (LLM).
2. Seed Personalized PageRank on the graph from those concepts.
3. Rerank candidates by the ACT-R activation equation: base-level (recency × frequency power-law decay) × spreading-activation contribution × embedding similarity × LLM-scored importance.
4. Return the top-N with their trails and community summaries attached.

One operation delivers both intentional traversal and serendipitous discovery.

**Write interface: MemGPT-style paging.** Agents pin, evict, search, and consolidate through explicit function calls. The CLI surface is small and verbs are cognitive-science-honest: `observe`, `recall`, `reflect`, `trail.begin`, `trail.end`, `pin`, `merge`. **No `link` verb** — linking is the substrate's job.

**Forgetting and reinforcement.** Base-level activation decays by a power law; successful retrievals reinforce. Stale observations are not deleted (accretion is preserved) but fall out of default retrieval rankings. This is the one thing Zettelkasten fundamentally cannot express — and it is also the one thing without which a long-lived multi-project knowledge store becomes a swamp.

### 3.3 What to drop from the current draft

- Mandatory authored typed links. Replaced by derived linking.
- `relates_to` as a catch-all edge. Forbidden; use typed frame slots or nothing.
- "Sequences" as a Zettelkasten concept. Replaced by Memex trails, which are strictly more expressive.
- Community detection as the *only* topic mechanism. Replaced by facets plus GraphRAG hierarchical summaries.
- Notes as the storage atom. Datoms are the atom; notes are views.
- The JSONL event log as a separate concern from the main store. The datom log *is* the store.

### 3.4 What to keep from Zettelkasten

- **Atomicity** of claims — one idea per claim. Now enforced by frame slots rather than by discipline.
- **No folder hierarchy.** Replaced by faceted classification, which is strictly better.
- **Emergence over top-down taxonomy** as an aesthetic commitment. Preserved via derived linking and GraphRAG communities.

### 3.5 The short version

**Cortex should be HippoRAG + CoALA + GraphRAG + Memex trails + Datomic + Ranganathan facets, not Zettelkasten.**

Every piece of that stack is load-bearing for a stated goal. Every piece has a peer-reviewed or production pedigree. Together they eliminate the linking-tax concern entirely by making linking a *derived property of the substrate* rather than an authoring obligation, and they give the system the two things the current draft has no answer for at all: **consolidation** (episodic → semantic via reflection) and **forgetting** (ACT-R decay and reinforcement).

---

## 4. Sources

- Edge, D. et al. (2024). *From Local to Global: A Graph RAG Approach to Query-Focused Summarization*. arxiv.org/abs/2404.16130
- Packer, C. et al. (2023). *MemGPT: Towards LLMs as Operating Systems*. arxiv.org/abs/2310.08560
- Xu, W. et al. (2025). *A-MEM: Agentic Memory for LLM Agents*. arxiv.org/abs/2502.12110 (NeurIPS 2025)
- Gutierrez, B. J. et al. (2024). *HippoRAG: Neurobiologically Inspired Long-Term Memory for Large Language Models*. arxiv.org/abs/2405.14831 (NeurIPS 2024)
- Park, J. S. et al. (2023). *Generative Agents: Interactive Simulacra of Human Behavior*. arxiv.org/abs/2304.03442
- Sumers, T. R. et al. (2023). *Cognitive Architectures for Language Agents*. arxiv.org/abs/2309.02427 (CoALA)
- McClelland, J. L., McNaughton, B. L. & O'Reilly, R. C. (1995). *Why There Are Complementary Learning Systems in the Hippocampus and Neocortex*. Psychological Review.
- Bush, V. (1945). *As We May Think*. The Atlantic. (Memex, trails.)
- Nelson, T. *Xanalogical Structure: Now More Than Ever*. (Transclusion, bidirectional links.)
- Ranganathan, S. R. (1933). *Colon Classification*. (PMEST, faceted classification.)
- ISO/IEC 13250-2:2006. *Information technology — Topic Maps — Data Model*.
- W3C. *SKOS Simple Knowledge Organization System Reference*. w3.org/TR/skos-reference/
- Minsky, M. (1974). *A Framework for Representing Knowledge*. MIT-AI Memo 306.
- Anderson, J. R. *ACT-R: A Theory of Higher Level Cognition*. (Activation equation.)
- Kanerva, P. (1988). *Sparse Distributed Memory*. MIT Press.
- Hickey, R. et al. *Datomic Information Model*. docs.datomic.com
- *Wikidata: Statements and Qualifiers*. wikidata.org/wiki/Help:Statements
