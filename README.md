# Cortex

**Cortex is a local knowledge substrate for AI coding agents.** It persists
observations, decisions, and discoveries as an append-only datom log and
projects them into a graph + vector store so agents can recall, reflect on,
and build on prior work across sessions.

Cortex is a single Go binary (`cortex`) that drives a managed stack of
Weaviate, Neo4j (with the Graph Data Science plugin), and a host-local
Ollama. Every write lands in a durable transaction log first; the backends
are rebuildable derived state.

This README covers Phase 1 of the system. For the full specification see
[`docs/spec/cortex-spec.md`](docs/spec/cortex-spec.md).

---

## What it does

- **Episodic capture** — `cortex observe` records observations, races,
  decisions with mandatory facets (domain, project, …) through a single
  validated write pipeline. Every write appends a sealed datom group to
  `~/.cortex/log.d/*.jsonl`, fsyncs once, and applies to Neo4j + Weaviate.
- **Retrieval & recall** — `cortex recall` runs the default HippoRAG +
  ACT-R path: concept extraction → seed resolution → Personalized
  PageRank over the semantic graph → entry load → ACT-R activation
  rerank → trail / community context attachment. Alternate retrieval
  modes described in the spec are specified but not yet exposed on the
  CLI in Phase 1.
- **Trails** — `cortex trail begin/end/show/list` groups a work session's
  observations into a replayable trail with an LLM-generated summary.
- **Reflection & analysis** — `cortex reflect` consolidates qualifying
  episodic clusters into semantic / procedural frames; `cortex analyze`
  detects cross-project patterns.
- **Ingestion** — `cortex ingest <path>` walks a repository, summarizes
  modules through Ollama, and writes episodic entries.
- **Rebuild & self-heal** — `cortex rebuild` replays the log into fresh
  Neo4j + Weaviate state; every read/write command silently replays any
  drift at startup (`--accept-drift` to re-embed under a new model).
- **Time-travel & lineage** — `cortex history <id>`, `cortex as-of <tx>`.
- **Operational verbs** — `cortex up/down/status/doctor/bench`.

The full command list lives in `cmd/cortex/commands.go`.

---

## Architecture at a glance

```
┌──────────────────────────────────────────────────────────────────┐
│                         cortex (Go binary)                        │
│  ┌─────────────────────────────────────────────────────────────┐  │
│  │  write pipeline: validate → secret-scan → resolve subject   │  │
│  │                 → embed → append(log) → apply(backends)     │  │
│  └─────────────────────────────────────────────────────────────┘  │
│                                │                                  │
│        ┌───────────────────────┼───────────────────────┐          │
│        ▼                       ▼                       ▼          │
│  ┌───────────┐         ┌───────────────┐      ┌──────────────┐    │
│  │ datom log │         │ Neo4j + GDS   │      │  Weaviate    │    │
│  │ ~/.cortex │◀────────│ (graph store) │      │ (vector idx) │    │
│  │  /log.d/  │ rebuild │ docker        │      │   docker     │    │
│  └───────────┘         └───────────────┘      └──────────────┘    │
│        │                                                          │
│        └── authoritative source of truth; self-heal replays       │
│            missing backend rows at startup                        │
│                                                                   │
│  ┌───────────────┐                                                │
│  │   Ollama      │ ← host service (not docker); embeddings +      │
│  │ (host-local)  │   link-derivation / reflection prompts         │
│  └───────────────┘                                                │
└──────────────────────────────────────────────────────────────────┘
```

Key invariants:
- The datom log is authoritative; backends are derived.
- Every write records the embedding model's name + digest, so
  `cortex rebuild` can detect pinned-model drift.
- All managed services bind loopback-only.

---

## Prerequisites

| Requirement     | Version                | Notes                                          |
|-----------------|------------------------|------------------------------------------------|
| **Go**          | 1.22 or later          | Needed to build the binary.                    |
| **Docker**      | with Compose v2        | Required for Weaviate + Neo4j.                 |
| **Ollama**      | recent                 | Runs on the host (not in Docker).              |
| **Ollama models** | `nomic-embed-text`, `qwen3:4b-instruct` | Pulled with `ollama pull`. |
| **Disk**        | ~8 GB free             | Docker images + persistent volumes.            |
| **Memory**      | 8 GB minimum, 16 GB recommended | Neo4j reserves ~3 GB, Weaviate ~2 GB. |
| **OS**          | macOS or Linux         | Phase 1 is developed on darwin/amd64 + linux.  |

### Bind ports (loopback only)

| Service      | Port(s)                    |
|--------------|----------------------------|
| Weaviate     | `127.0.0.1:9397` (HTTP), `127.0.0.1:50051` (gRPC) |
| Neo4j Bolt   | `127.0.0.1:7687`           |
| Neo4j HTTP   | `127.0.0.1:7474`           |
| Ollama       | `127.0.0.1:11434`          |

Nothing is exposed on `0.0.0.0`.

---

## Install

### 1. Install Go, Docker, and Ollama

```bash
# Go: https://go.dev/dl/  (1.22+)
# Docker Desktop / Docker Engine with Compose v2
# Ollama: https://ollama.com/download
```

### 2. Pull the required Ollama models

```bash
ollama pull nomic-embed-text       # embedding model (768-dim)
ollama pull qwen3:4b-instruct      # generation model for link derivation + reflection
```

Make sure `ollama serve` is running (Ollama Desktop starts it automatically;
on Linux run `systemctl --user start ollama` or `ollama serve &`).

### 3. Build the Cortex binary

```bash
git clone https://github.com/nixlim/cortex.git
cd cortex
go build -o cortex ./cmd/cortex
```

Install it somewhere on `$PATH`:

```bash
install -m 0755 cortex ~/bin/cortex
# or
sudo install -m 0755 cortex /usr/local/bin/cortex
```

Verify:

```bash
cortex version
```

### 4. Bring up the managed stack

```bash
cortex up
```

On first run, `cortex up`:

1. Validates Docker is reachable.
2. Generates a random Neo4j password and persists it to `~/.cortex/config.yaml` (mode 0600).
3. Builds the custom `cortex/neo4j-gds:0.1.0` image (Neo4j 5.24 + GDS 2.13.2). This step takes a few minutes on first run only.
4. Starts Weaviate and Neo4j via `docker compose -f docker/docker-compose.yaml up -d`.
5. Probes Weaviate, Neo4j, Neo4j GDS, and Ollama for readiness within a 90-second budget.
6. Confirms the required Ollama models are installed.

Expected output on success:

```
cortex up  managed stack is ready
```

If any probe fails, `cortex up` exits with a stable error code
(`DOCKER_UNREACHABLE`, `NEO4J_NOT_READY`, `OLLAMA_MODEL_MISSING`, …) and a
remediation hint. See the `infra.Code*` constants in
`internal/infra/up.go` for the full list.

---

## Quick start

After `cortex up` succeeds:

```bash
# 1. Write an observation
cortex observe "Retries must use exponential backoff with full jitter" \
  --kind=Observation \
  --facets=domain:Reliability,project:pay-gw

# 2. Recall it
cortex recall "retry strategy" --json

# 3. Capture a work session as a trail
export CORTEX_TRAIL_ID=$(cortex trail begin --agent=my-agent --name="debug retry storm")
cortex observe "Saw thundering herd at 09:42" --kind=ObservedRace \
  --facets=domain:Reliability,project:pay-gw
cortex observe "Root cause: no jitter on client retry" --kind=Observation \
  --facets=domain:Reliability,project:pay-gw
cortex trail end

# 4. Consolidate patterns into frames
cortex reflect

# 5. Inspect the stack
cortex status
cortex doctor
```

### Shut down

```bash
cortex down              # stop containers; keep volumes (data preserved)
cortex down --purge      # interactive prompt: also remove volumes (data lost)
```

---

## Trails and `CORTEX_TRAIL_ID`

A **trail** is the envelope that groups a work session's observations into
a single replayable bundle. `cortex trail end` asks the host generation
model to synthesize a short narrative summary over the trail's entries,
which gives future recalls a thread to follow instead of a scatter of
individual datoms. Trails are optional — standalone `cortex observe`
calls work fine — but they are the right tool any time you expect the
work you are about to do to produce more than a single persistent fact.

### The contract

`cortex trail begin` prints a new `trail:<ulid>` to stdout **and nothing
else**, so you capture it into a shell variable named `CORTEX_TRAIL_ID`.
From that moment on:

- Every `cortex observe` in the same environment auto-attaches to the
  trail via the `--trail` fallback — you do not pass the flag.
- `cortex trail end` reads `CORTEX_TRAIL_ID` from the environment,
  materializes the trail's entries, runs the summary prompt, and writes
  `ended_at` + `summary` datoms. If the variable is unset it exits `2`
  with code `NO_ACTIVE_TRAIL`.
- `CORTEX_TRAIL_ID` is the **only** environment variable Cortex reads.

```bash
export CORTEX_TRAIL_ID=$(cortex trail begin \
  --agent=claude-code \
  --name="debug retry storm")

cortex observe "Saw thundering herd at 09:42"        --kind=ObservedRace --facets=domain:Reliability,project:pay-gw
cortex observe "Root cause: no jitter on client retry" --kind=Observation  --facets=domain:Reliability,project:pay-gw

cortex trail end
unset CORTEX_TRAIL_ID
```

You can override the trail attachment on any single `observe` by passing
`--trail=<id>` explicitly; the flag wins over the environment variable.

### When to begin a trail

Begin a trail when the work you are about to do is likely to produce
**multiple related observations** that a future query should surface
together. Good triggers:

- Debugging a non-trivial bug (root cause + attempted fixes + final fix).
- Implementing a feature where the design rationale and the concrete
  implementation choices should stay bundled.
- Running a spike, audit, or benchmark that you want future agents to
  find as one coherent thread.
- A task that will span more than a few minutes of active work.

Do **not** begin a trail when:

- You are recording a single standalone observation. Just run
  `cortex observe` on its own.
- The work is pure exploration with nothing worth persisting.
- A trail is already active in the environment — check `echo
  $CORTEX_TRAIL_ID` first. Nested trails are not supported.
- You cannot guarantee you will call `cortex trail end` (e.g. inside a
  short-lived CI step where the process may exit abruptly).

### When to end a trail

Run `cortex trail end` before:

- Claiming the task you opened the trail for is complete.
- Switching to an unrelated task.
- The end of a working session.

A trail that is never ended is still valid data — the datoms are in the
log — but the LLM narrative summary only runs at end time, so un-ended
trails never appear in recall with their trail-level context. Always end
trails you open.

### Inspecting trails

```bash
cortex trail list                    # reverse-chronological list of trails
cortex trail show trail:<ulid>       # one trail, with its member entries
```

`cortex recall` surfaces the trail summary automatically when any of the
trail's member entries scores high enough, via the `TrailContext` field
on each result.

---

## Commands

| Command           | What it does                                                    |
|-------------------|-----------------------------------------------------------------|
| `cortex up`       | Start the managed stack; enforce the readiness contract.        |
| `cortex down`     | Stop the stack; `--purge` additionally drops volumes.           |
| `cortex status`   | Print per-service health and watermark summary.                 |
| `cortex doctor`   | Run diagnostic checks against the live stack.                   |
| `cortex observe`  | Write a validated episodic entry.                               |
| `cortex recall`   | Retrieve entries using HippoRAG + ACT-R default-mode retrieval. Flags: `--limit`, `--json`. |
| `cortex trail`    | `begin/end/show/list` work sessions.                            |
| `cortex history`  | Show the full retract-aware lineage of an entity.               |
| `cortex as-of`    | Run a query against a historical transaction id.                |
| `cortex reflect`  | Consolidate episodic clusters into frames.                      |
| `cortex analyze`  | Cross-project pattern analysis.                                 |
| `cortex ingest`   | Walk a repository into module-level episodic entries.           |
| `cortex rebuild`  | Replay the log into fresh backend state.                        |
| `cortex merge`    | Merge external log segments into `~/.cortex/log.d`.              |
| `cortex retract`  | Write a retraction datom against an entity.                     |
| `cortex subject`  | Manage PSI subjects (merge / alias).                            |
| `cortex community`, `cortex communities` | Inspect Leiden communities.          |
| `cortex pin` / `unpin` / `evict` / `unevict` | Activation overrides.            |
| `cortex export`   | Merge all segments into one tx-sorted stream.                   |
| `cortex migrate`  | Migrate content from an external knowledge system.              |
| `cortex bench`    | Phase 1 benchmark suite (`--profile`, `--corpus`, `--live`).    |
| `cortex version`  | Print the Cortex version.                                       |

Every command supports `--json` for machine-readable output and follows the
standard exit codes (`0` success, `1` operational, `2` validation).

---

## Configuration

Cortex reads `~/.cortex/config.yaml` on every invocation. The file is
auto-created by `cortex up` on first run with secure defaults; every
field maps to a typed Go struct in `internal/config/defaults.go`.

Notable knobs:

```yaml
retrieval:
  default_limit: 10
  ppr:
    seed_top_k: 10
    damping: 0.85
    max_iterations: 30
  activation:
    decay_exponent: 0.5
    weights:
      base_level: 0.4
      ppr: 0.3
      similarity: 0.2
      importance: 0.1
  forgetting:
    visibility_threshold: 0.05

link_derivation:
  confidence_floor: 0.60
  similar_to_cosine_floor: 0.75

endpoints:
  weaviate_http: localhost:9397
  weaviate_grpc: localhost:50051
  neo4j_bolt:    localhost:7687
  ollama:        localhost:11434

ollama:
  num_ctx: 32768                # context window for /api/generate
  embedding_vector_dim: 768     # expected embedding length; drift → EMBEDDING_DIM_MISMATCH

ingest:
  ollama_concurrency: 2         # max concurrent module summary calls against Ollama
  module_size_limit_bytes: 262144

timeouts:
  ingest_summary_seconds: 600   # per-module wall clock for the structured-output summarizer

neo4j_password: <auto-generated, mode 0600>
```

The config path is fixed at `~/.cortex/config.yaml`; there is no env-var
override. The only environment variable Cortex reads is
`CORTEX_TRAIL_ID`, which lets `cortex observe` automatically attach
entries to an active trail without passing `--trail` every time.

---

## Data and log layout

```
~/.cortex/
├── config.yaml           # 0600 — operator-local config + credentials
├── log.d/                # append-only datom segments
│   ├── 0000000001.jsonl
│   ├── 0000000002.jsonl
│   └── .quarantine/      # segments that failed checksum validation
├── ops.log               # structured command audit (JSONL)
└── bench/latest.json     # last benchmark report
```

Docker-managed persistent state:

```
cortex_weaviate_data   # Weaviate objects + vectors
cortex_neo4j_data      # Neo4j graph database
```

`cortex down` preserves both volumes. `cortex down --purge` prompts before
deleting them.

---

## Development

### Build & test

```bash
go build ./...                # compile everything
go test ./...                 # full unit + e2e suite
go test ./internal/write/...  # package-scoped
go vet ./...                  # vet
```

End-to-end tests live in `internal/e2e/`. The default `go test ./...`
run is hermetic. A stricter integration suite that exercises the
real binary against a live stack lives behind build tags:

```bash
go test -tags='cli' ./internal/e2e/...               # CLI-exec, still hermetic
go test -tags='cli integration' ./internal/e2e/...   # requires `cortex up` to be running
```

### Code intelligence (optional)

The repo is indexed by [GitNexus](https://github.com/nixlim/gitnexus)
for semantic code navigation:

```bash
npx gitnexus analyze --embeddings   # rebuild index after commits
```

A PostToolUse hook auto-runs this after `git commit` when using Claude Code.

### Issue tracking (optional)

This project uses [beads (bd)](https://github.com/nixlim/beads) for
local, git-versioned issue tracking. The workflow is documented in
`CLAUDE.md`. Beads is not required to build or run Cortex.

### Project layout

```
cmd/cortex/            # CLI entrypoint + per-subcommand wiring
internal/
  config/              # ~/.cortex/config.yaml schema + loader
  datom/               # datom shape, sealing, UUID derivation
  log/                 # append-only segmented JSONL log
  write/               # write pipeline (validate → seal → apply)
  recall/              # HippoRAG + ACT-R read pipeline
  reflect/             # episodic → semantic frame consolidation
  analyze/             # cross-project pattern analysis
  ingest/              # repository walker + module summarizer
  rebuild/             # log → backend rebuild loop
  replay/              # startup self-heal replay
  neo4j/               # Bolt adapter + BackendApplier
  weaviate/            # HTTP adapter + BackendApplier + staging classes
  ollama/              # HTTP adapter: Embed, Generate, Show
  infra/               # `cortex up/down/status/doctor` orchestration
  bench/               # benchmark harness
  e2e/                 # end-to-end integration tests
docker/
  docker-compose.yaml  # Weaviate + Neo4j services
  neo4j-gds/           # Dockerfile for the custom Neo4j + GDS image
docs/spec/             # full Phase 1 specification
```

---

## Troubleshooting

| Symptom                                      | Action                                                                 |
|----------------------------------------------|------------------------------------------------------------------------|
| `cortex up` reports `OLLAMA_MODEL_MISSING`   | `ollama pull nomic-embed-text && ollama pull qwen3:4b-instruct`         |
| `DOCKER_UNREACHABLE`                         | Start Docker Desktop / `systemctl start docker`.                        |
| `NEO4J_NOT_READY` with auth errors on a fresh machine | Fixed in cortex-54l (ComposeUp now threads `NEO4J_PASSWORD`). If you hit this on an older build, `docker volume rm cortex_neo4j_data && cortex up`. |
| `STARTUP_BUDGET_EXCEEDED`                    | Slow host; retry, or edit `StartupBudget` in wiring. Check `cortex doctor` for per-service diagnostics. |
| `EMBEDDING_DIM_MISMATCH`                     | Your embedder returns a different dim than `ollama.embedding_vector_dim` in config. Either fix the model or `cortex rebuild --accept-drift`. |
| Want to reset everything                     | `cortex down --purge && rm -rf ~/.cortex && cortex up`                  |

Run `cortex doctor` any time the stack looks unhealthy — it runs the same
probes `cortex up` uses and reports each one individually.

---

## License

See the project repository for license terms.
