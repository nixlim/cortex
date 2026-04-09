 Two things worth flagging before ship

  1. SC-019 (cross-operator reproducibility) is almost but
  not quite provable by CI. Two CI runners on the same
  commit should produce identical Layer 1/2 state. Layer 3
  (embeddings) depends on the Ollama model digest matching,
  which requires both runners to have pulled the same
  digest. Since CI typically pulls fresh on each run, the
  digests should agree, but any Ollama registry mutation
  during a CI window breaks this silently. Worth considering
   whether the CI pipeline should pin the Ollama model
  digest via ollama pull nomic-embed-text@<digest> rather
  than by tag. Not a spec defect; a CI pipeline
  configuration note.
  2. The custom image tag
  cortex/neo4j-gds:<cortex-release-version> binds the Neo4j
  image lifecycle to the Cortex release lifecycle. That's
  deliberate and correct — it means cortex 0.3 → 0.4 is a
  single atomic upgrade across the CLI binary and the Neo4j
  image. But it also means a Cortex release that doesn't
  change Neo4j or GDS still produces a new image tag, and
  every operator's first cortex up after the release
  triggers a fresh docker build. For small version bumps
  this is wasteful but not wrong. An optimization worth
  considering later (not now): tag the image by
  neo4j-version + gds-version + cortex-infra-version hash
  instead of the Cortex release version, so unchanged images
   are cache hits across releases. Phase 1 doesn't need
  this; Phase 2 might.

  Neither is a blocker. The spec is ready for implementation
   on the substrate, write path, retrieval, reflection,
  ingestion, and infrastructure fronts.
