# Cortex UI Spec

## Goal

Build a local, read-only web UI for visually exploring the Cortex memory graph
using Sigma.js. The UI should let a human start from a recall query, a
community, or a known node, then expand the graph incrementally without dumping
the entire Neo4j graph.

## Non-Goals for MVP

- No graph editing, retraction, merge, pin, evict, or write operations.
- No arbitrary Cypher from the browser.
- No global full-graph render.
- No hosted or multi-user auth story initially; bind locally by default.

## Primary User Flows

### Recall to Graph

The user enters a query. Cortex runs recall with a configurable limit. The UI
renders returned entries as focus nodes, then fetches a bounded neighborhood
around them.

The node detail panel should show:

- Body preview and full body on demand.
- Recall score.
- Base activation.
- PPR score.
- Similarity score.
- Why-surfaced trace.
- Trail context.
- Community context.
- Entry facets and timestamps.

### Community Map

The user opens a Communities view. Cortex returns community nodes by hierarchy
level. The UI renders communities as aggregate bubbles sized by member count.
Clicking a community expands member entries and important concepts or trails.

### Node Neighborhood

The user clicks any node and chooses Expand. Cortex returns a depth-limited
neighborhood filtered by node type, edge type, and max node count. The UI merges
new nodes and edges into the current Sigma graph rather than replacing the whole
view.

### Path Explorer

The user selects two nodes and asks for a path. Cortex returns the shortest path
nodes and typed edges. The UI highlights that path and dims unrelated nodes.

## Frontend

Use Sigma.js with graphology.

Suggested source layout:

```text
web/graph-ui/
```

UI layout:

- Top search bar: recall query and node ID search.
- Left filters: node types, edge types, project/domain, activation threshold,
  recency, and community level.
- Center: Sigma canvas.
- Right detail panel: selected node or edge details, expand action, and path
  actions.
- Bottom status strip: visible node count, edge count, query latency, and
  backend status.

## Visual Encoding

Node color should be based on node type:

- Entry.
- Frame.
- Subject or PSI.
- Trail.
- Community.
- Concept.

Node size should be based on context:

- Recall score when opened from search.
- Member count for communities.
- Degree or activation for general exploration.

Edge color or style should be based on relationship type:

- MENTIONS.
- ABOUT.
- IN_TRAIL.
- IN_COMMUNITY.
- SIMILAR_TO.
- DERIVED_FROM.
- SUPERSEDES.
- ALIAS_OF.

Edge thickness should use confidence or weight where available. If no weight is
available, use a uniform width.

Labels should be hidden by default except for selected, hovered, focus, and
high-importance nodes.

## Backend

Add a local read-only HTTP server:

```text
cortex ui [--addr=127.0.0.1:8765] [--open=false]
```

Suggested package layout:

```text
cmd/cortex/ui.go
internal/graphui/server.go
internal/graphui/queries.go
internal/graphui/types.go
web/graph-ui/
```

The backend should reuse Cortex config for Neo4j credentials, matching current
CLI read paths.

## API Sketch

```text
GET  /api/status
POST /api/recall
GET  /api/node/:id
GET  /api/neighborhood?id=<id>&depth=1&limit=150
GET  /api/path?from=<id>&to=<id>&limit=50
GET  /api/communities?level=0
GET  /api/community/:id
```

The browser must not send arbitrary Cypher. Every API should map to a fixed,
parameterized backend query.

## Graph Response Shape

```json
{
  "nodes": [
    {
      "id": "entry:...",
      "type": "Entry",
      "label": "Short title or body preview",
      "summary": "Optional summary",
      "body_preview": "First 240 chars",
      "properties": {
        "project": "cortex",
        "domain": "code",
        "activation": 0.73,
        "created_at": "2026-04-11T00:00:00Z"
      }
    }
  ],
  "edges": [
    {
      "id": "entry:...->concept:recall:MENTIONS",
      "source": "entry:...",
      "target": "concept:recall",
      "type": "MENTIONS",
      "weight": 1.0,
      "properties": {}
    }
  ],
  "meta": {
    "truncated": false,
    "limit": 150,
    "latency_ms": 42
  }
}
```

## Backend Query Rules

- Use parameterized queries only.
- Apply a hard cap to returned nodes and edges.
- Default to depth 1.
- Allow depth 2.
- Reject deeper traversal for MVP.
- Exclude retracted and evicted nodes by default.
- Add an explicit UI toggle later for hidden/retracted/evicted content.
- Support type filters server-side so the frontend does not receive excess data.
- Do not return full bodies in graph list responses by default.
- Fetch full node detail through `/api/node/:id`.

## Performance

- Initial render target: fewer than 300 nodes and fewer than 1,000 edges.
- Prefer expansion over full graph refresh.
- Cache node detail responses during a UI session.
- Use a graphology layout worker where practical.
- Stop layout once the graph stabilizes.
- For larger graphs, show community aggregates first, then expand.

## Security and Privacy

- Bind to `127.0.0.1` by default.
- Keep MVP read-only.
- Do not expose arbitrary Cypher.
- Redact or truncate large and sensitive text fields in graph responses.
- Return full body text only through explicit node detail fetches.
- Reuse Cortex config for Neo4j credentials.

## MVP Acceptance Criteria

- `cortex ui` starts a local server and serves the Sigma.js UI.
- A recall query renders top entries and their immediate graph neighborhood.
- Clicking a node shows details and can expand one hop.
- The community list renders aggregate community nodes and expands members.
- The path explorer highlights a shortest path between two selected nodes.
- The UI remains responsive with 300 nodes and 1,000 edges.
- Backend tests cover parameterized neighborhood, community, node detail, and
  path responses.
