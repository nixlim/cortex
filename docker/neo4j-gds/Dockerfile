# syntax=docker/dockerfile:1.7
###############################################################################
# Cortex custom Neo4j + Graph Data Science image.
#
# Spec references:
#   docs/spec/cortex-spec.md §"Custom Neo4j + GDS Image"
#   docs/spec/cortex-spec.md §"Pinned Service Versions"
#   docs/spec/cortex-spec.md §"Resource Allocation"
#
# This Dockerfile pre-bakes the Graph Data Science plugin into the Neo4j
# community image. Baking (rather than installing on first boot) avoids a
# timing race between Neo4j startup and plugin loading, and lets
# `cortex doctor` report a deterministic plugin state.
#
# Image tag (set by docker-compose.yaml):
#   cortex/neo4j-gds:<cortex-release-version>
#
# The tag is bound to the Cortex release, not Neo4j or GDS versions, so the
# binary and its Neo4j image upgrade as a unit. Cortex does NOT push this
# image to any remote registry; each operator's machine builds it from
# upstream sources at `cortex up` time.
#
# Offline builds: pass --build-arg GDS_LOCAL_PATH=/abs/path/to/gds.jar to
# skip the network download and use a pre-staged jar instead.
###############################################################################

ARG NEO4J_BASE=neo4j:5.24-community
FROM ${NEO4J_BASE}

# ----------------------------------------------------------------------------
# Build-time arguments
# ----------------------------------------------------------------------------
# GDS release pinned to a specific version. Keep this in lockstep with the
# docker-compose.yaml `GDS_VERSION` build arg and with the value documented
# in the Pinned Service Versions table.
ARG GDS_VERSION=2.13.2

# Optional path to a pre-downloaded GDS jar on the build context. If set,
# the build copies the local jar instead of fetching from the network. Used
# for air-gapped builds.
ARG GDS_LOCAL_PATH=""

# Canonical upstream download URL for the community edition release.
ARG GDS_DOWNLOAD_URL="https://github.com/neo4j/graph-data-science/releases/download/${GDS_VERSION}/neo4j-graph-data-science-${GDS_VERSION}.jar"

# ----------------------------------------------------------------------------
# Install the GDS plugin
# ----------------------------------------------------------------------------
# The upstream neo4j image runs as user `neo4j` and expects plugins at
# /var/lib/neo4j/plugins/. We copy or download the jar, rename it to the
# canonical `gds-<version>.jar` form so `cortex doctor` can find it by a
# stable path, and fix ownership.
#
# We switch to root for the install step, then switch back before CMD.
USER root

# curl is not guaranteed in all upstream neo4j tags — install it only if
# we actually need it (i.e., no local jar was provided).
RUN set -eux; \
    if [ -z "${GDS_LOCAL_PATH}" ]; then \
        apt-get update; \
        apt-get install -y --no-install-recommends curl ca-certificates; \
        rm -rf /var/lib/apt/lists/*; \
    fi

# Two-path plugin install: local copy (via a BuildKit bind mount against
# the build context) or network fetch from upstream. The resulting jar
# always lands at /var/lib/neo4j/plugins/gds-<version>.jar with neo4j
# ownership, so the acceptance check (`gds-<version>.jar present in the
# plugins dir`) is deterministic regardless of which install path was
# taken.
#
# The bind-mount (`--mount=type=bind,target=/build-ctx`) exposes the
# docker/neo4j-gds build context read-only without requiring a COPY,
# which sidesteps the "COPY glob must match at least one file" trap when
# no local jar is present. Requires BuildKit (default in Docker >= 23).
RUN --mount=type=bind,target=/build-ctx,readonly \
    set -eux; \
    mkdir -p /var/lib/neo4j/plugins; \
    if [ -n "${GDS_LOCAL_PATH}" ] && [ -f "${GDS_LOCAL_PATH}" ]; then \
        # Operator-supplied absolute path (BuildKit secret mount or
        # pre-populated image layer).
        cp "${GDS_LOCAL_PATH}" "/var/lib/neo4j/plugins/gds-${GDS_VERSION}.jar"; \
    elif ls /build-ctx/gds-local*.jar >/dev/null 2>&1; then \
        # Operator dropped a jar into docker/neo4j-gds/ named gds-local*.jar
        # — picked up for air-gapped builds.
        cp /build-ctx/gds-local*.jar "/var/lib/neo4j/plugins/gds-${GDS_VERSION}.jar"; \
    else \
        # Default: download from the upstream GDS release URL.
        curl -fsSL -o "/var/lib/neo4j/plugins/gds-${GDS_VERSION}.jar" "${GDS_DOWNLOAD_URL}"; \
    fi; \
    chown neo4j:neo4j "/var/lib/neo4j/plugins/gds-${GDS_VERSION}.jar"; \
    chmod 0644 "/var/lib/neo4j/plugins/gds-${GDS_VERSION}.jar"

# ----------------------------------------------------------------------------
# Neo4j configuration overlay
# ----------------------------------------------------------------------------
# We ship a neo4j.conf overlay that enables the GDS procedure allowlist.
# The upstream image merges NEO4J_* environment variables into neo4j.conf
# at container start, so environment-based config (set in docker-compose
# and by `cortex up` at runtime) remains authoritative. This file exists
# for keys that are awkward to express as environment variables.
COPY neo4j.conf /var/lib/neo4j/conf/neo4j.conf
RUN chown neo4j:neo4j /var/lib/neo4j/conf/neo4j.conf && \
    chmod 0644 /var/lib/neo4j/conf/neo4j.conf

# ----------------------------------------------------------------------------
# Healthcheck — a Cypher ping via cypher-shell.
# ----------------------------------------------------------------------------
# The healthcheck uses the NEO4J_PASSWORD env var so it works with whatever
# credential `cortex up` injected at runtime. If the variable is unset, the
# check falls back to the bootstrap default used by the compose file.
#
# Exit non-zero until the database accepts queries, per spec:
#   "image declares a Docker HEALTHCHECK that runs a Cypher ping (RETURN 1)
#    via cypher-shell and exits non-zero until the database accepts
#    queries."
HEALTHCHECK --interval=2s --timeout=5s --retries=30 --start-period=10s \
    CMD cypher-shell -u neo4j -p "${NEO4J_PASSWORD:-cortexbootstrap}" "RETURN 1" >/dev/null 2>&1 || exit 1

# Drop back to the unprivileged user used by the upstream image for the
# actual server process.
USER neo4j
