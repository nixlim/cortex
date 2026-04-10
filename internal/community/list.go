// community/list.go is the read side of the community subsystem,
// powering `cortex communities` and `cortex community show`. The
// detect.go path persists Community nodes via Persist; this file
// reads those nodes back through the QueryGraph seam so the CLI can
// display them.
//
// Spec references:
//
//	docs/spec/cortex-spec.md FR-029 / SC-013 (community CLI surfaces
//	  hierarchical communities and their summaries)
package community

import (
	"context"
	"fmt"

	"github.com/nixlim/cortex/internal/errs"
)

// Reader is the narrow read-only seam ListCommunities and
// ShowCommunity need from the neo4j adapter. *neo4j.BoltClient already
// satisfies it via its QueryGraph method; tests pass a fake reader
// that returns canned rows so they do not need a live database.
type Reader interface {
	QueryGraph(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)
}

// Listed is one community summary in a `cortex communities` response.
// EntryCount is named for clarity in the JSON output even though the
// Persist phase calls the underlying property "member_count".
type Listed struct {
	ID          int64  `json:"id"`
	Level       int    `json:"level"`
	MemberCount int    `json:"member_count"`
	Summary     string `json:"summary,omitempty"`
}

// Detail is the row shape `cortex community show <id>` returns. The
// extra MemberIDs slice is the entry-prefixed ULID list resolved
// through the IN_COMMUNITY edges.
type Detail struct {
	ID          int64    `json:"id"`
	Level       int      `json:"level"`
	MemberCount int      `json:"member_count"`
	Summary     string   `json:"summary,omitempty"`
	MemberIDs   []string `json:"member_ids"`
}

// MinimumCommunitySize is the floor below which a community is
// considered noise and is suppressed from default listings. The spec
// AC ("returns an empty list with exit zero when the graph is below
// minimum community size") is satisfied by filtering out anything
// smaller than this floor.
const MinimumCommunitySize = 2

// ErrCommunityNotFound is the operational NOT_FOUND error returned by
// ShowCommunity when no Community node exists for the requested
// (level, id) tuple. The CLI maps this to exit 1 per the spec
// acceptance criterion "community show on a missing id exits 1 with
// NOT_FOUND".
var ErrCommunityNotFound = errs.Operational("NOT_FOUND",
	"no community matches the requested id and level", nil)

// ListCommunities returns every persisted Community at the requested
// level whose member count meets MinimumCommunitySize. Results are
// sorted by community id ascending so the JSON output is stable
// across runs.
func ListCommunities(ctx context.Context, r Reader, level int) ([]Listed, error) {
	if r == nil {
		return nil, errs.Operational("NEO4J_UNAVAILABLE",
			"community list called without a neo4j reader", nil)
	}
	const cypher = `
MATCH (c:Community {level: $level})
RETURN c.community_id AS community_id,
       c.member_count AS member_count,
       c.summary AS summary
ORDER BY c.community_id ASC
`
	rows, err := r.QueryGraph(ctx, cypher, map[string]any{"level": int64(level)})
	if err != nil {
		return nil, errs.Operational("NEO4J_QUERY_FAILED",
			"failed to query communities from neo4j", err)
	}
	out := make([]Listed, 0, len(rows))
	for _, row := range rows {
		id, ok := rowInt64(row, "community_id")
		if !ok {
			continue
		}
		mc, _ := rowInt64(row, "member_count")
		if int(mc) < MinimumCommunitySize {
			continue
		}
		summary, _ := row["summary"].(string)
		out = append(out, Listed{
			ID:          id,
			Level:       level,
			MemberCount: int(mc),
			Summary:     summary,
		})
	}
	return out, nil
}

// ShowCommunity returns the full detail row for one community. The
// implementation issues two Cypher queries: the first fetches the
// Community node properties (and proves existence), the second
// resolves member entry ids via the IN_COMMUNITY edges.
//
// A missing community node returns ErrCommunityNotFound. Member id
// resolution is best-effort: a community with no member entries
// (which is technically allowed by the persistence schema if the
// IN_COMMUNITY edges have been retracted) returns Detail with an
// empty MemberIDs slice rather than NOT_FOUND.
func ShowCommunity(ctx context.Context, r Reader, level int, id int64) (*Detail, error) {
	if r == nil {
		return nil, errs.Operational("NEO4J_UNAVAILABLE",
			"community show called without a neo4j reader", nil)
	}
	const headerCypher = `
MATCH (c:Community {level: $level, community_id: $id})
RETURN c.member_count AS member_count, c.summary AS summary
LIMIT 1
`
	headerRows, err := r.QueryGraph(ctx, headerCypher, map[string]any{
		"level": int64(level),
		"id":    id,
	})
	if err != nil {
		return nil, errs.Operational("NEO4J_QUERY_FAILED",
			"failed to query community header", err)
	}
	if len(headerRows) == 0 {
		return nil, ErrCommunityNotFound
	}
	mc, _ := rowInt64(headerRows[0], "member_count")
	summary, _ := headerRows[0]["summary"].(string)

	const memberCypher = `
MATCH (c:Community {level: $level, community_id: $id})<-[:IN_COMMUNITY]-(n)
RETURN n.entry_id AS entry_id
ORDER BY n.entry_id ASC
`
	memberRows, err := r.QueryGraph(ctx, memberCypher, map[string]any{
		"level": int64(level),
		"id":    id,
	})
	if err != nil {
		return nil, errs.Operational("NEO4J_QUERY_FAILED",
			"failed to query community members", err)
	}
	members := make([]string, 0, len(memberRows))
	for _, row := range memberRows {
		if eid, ok := row["entry_id"].(string); ok && eid != "" {
			members = append(members, eid)
		}
	}
	return &Detail{
		ID:          id,
		Level:       level,
		MemberCount: int(mc),
		Summary:     summary,
		MemberIDs:   members,
	}, nil
}

// FormatID renders a (level, id) tuple as the canonical CLI string
// "L<level>:C<id>". The format is symmetric with ParseID below and
// keeps level and id together so the user can copy a single token
// from `cortex communities` output into `cortex community show`.
func FormatID(level int, id int64) string {
	return fmt.Sprintf("L%d:C%d", level, id)
}

// ParseID is the inverse of FormatID. It accepts the canonical
// "L<level>:C<id>" form and returns the parsed level and id. A bare
// numeric id is rejected so the user gets a clear error rather than
// a confusing NOT_FOUND.
func ParseID(s string) (level int, id int64, err error) {
	var l int
	var c int64
	n, scanErr := fmt.Sscanf(s, "L%d:C%d", &l, &c)
	if scanErr != nil || n != 2 {
		return 0, 0, fmt.Errorf("expected community id of the form L<level>:C<id>, got %q", s)
	}
	return l, c, nil
}
