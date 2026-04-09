package community

import (
	"context"
	"fmt"
)

// Summarizer produces a short natural-language description of a
// community given its member IDs. In production this is backed by
// the ollama adapter's Generate method; tests substitute a
// deterministic fake so the refresh logic can be exercised without
// an LLM round-trip.
type Summarizer interface {
	Summarize(ctx context.Context, community Community) (string, error)
}

// Refresher regenerates community summaries after a reflection or
// analyze pass. The critical acceptance criterion is that a refresh
// only calls the Summarizer for communities whose membership
// actually changed — LLM calls are the most expensive part of the
// pipeline and re-running them for untouched communities would
// dominate the reflection budget.
type Refresher struct {
	Neo4j      Neo4jClient
	Summarizer Summarizer
}

// Refresh walks the new hierarchy, compares each community's member
// set against the prior hierarchy, and invokes the Summarizer only
// for communities whose membership differs. It returns the updated
// hierarchy (with Summary populated) and the set of (level,
// communityId) pairs that were actually regenerated so callers can
// log the ratio for observability.
func (r *Refresher) Refresh(
	ctx context.Context,
	prior [][]Community,
	next [][]Community,
) ([][]Community, []Key, error) {
	if r.Summarizer == nil {
		return nil, nil, fmt.Errorf("community: no summarizer configured")
	}

	priorIndex := indexByKey(prior)
	var regenerated []Key

	out := make([][]Community, len(next))
	for level, communities := range next {
		out[level] = make([]Community, len(communities))
		for i, c := range communities {
			key := Key{Level: c.Level, ID: c.ID}
			prev, existed := priorIndex[key]
			if existed && sameMembership(prev.Members, c.Members) {
				// Unchanged: carry forward the cached summary
				// without calling the Summarizer.
				c.Summary = prev.Summary
				out[level][i] = c
				continue
			}
			summary, err := r.Summarizer.Summarize(ctx, c)
			if err != nil {
				return nil, nil, fmt.Errorf("community: summarize level %d id %d: %w", c.Level, c.ID, err)
			}
			c.Summary = summary
			out[level][i] = c
			regenerated = append(regenerated, key)
		}
	}
	return out, regenerated, nil
}

// Key identifies a community within a hierarchy. GDS does not
// guarantee stable IDs across runs, but within a single detection
// pass the (level, id) pair is unique, which is all Refresh needs
// for its diff.
type Key struct {
	Level int
	ID    int64
}

func indexByKey(hierarchy [][]Community) map[Key]Community {
	out := map[Key]Community{}
	for _, level := range hierarchy {
		for _, c := range level {
			out[Key{Level: c.Level, ID: c.ID}] = c
		}
	}
	return out
}

// sameMembership reports whether two member-ID lists represent the
// same set. We don't assume the caller sorted them — GDS streams
// nodes in an unspecified order — so we compare as sets via a small
// map. The lists are typically small (dozens to low thousands) so
// this is cheap.
func sameMembership(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[int64]struct{}, len(a))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}
