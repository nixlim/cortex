// Link derivation (A-MEM style) for the Cortex write pipeline.
//
// After a successful observe commit, the pipeline asks an LLM-backed
// proposer to score a handful of candidate neighbors returned by
// Weaviate's nearest-neighbor search. Only proposals clearing the
// confidence floor (and, for SIMILAR_TO, an additional cosine floor)
// are persisted as link datoms attached to the source entry.
//
// The module is deliberately free of any log or adapter dependency so
// that AC4 — "link derivation executes outside the log flock" — is
// satisfied by construction: the functions in this file cannot acquire
// the log flock because they never touch the log writer. The caller
// (the CLI wiring) is responsible for running DeriveLinks after
// Pipeline.Observe returns, i.e., after the append flock has already
// been released.
//
// Spec references:
//   docs/spec/cortex-spec.md §"A-MEM Link Derivation"
//   docs/spec/cortex-spec.md §"Configuration Defaults" (link_derivation.*)
//   bead cortex-4kq.41
package write

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nixlim/cortex/internal/datom"
)

// LinkTypeSimilarTo is the canonical A-MEM link type that gets the
// extra cosine floor on top of the generic confidence floor. All other
// link types (CAUSES, REFINES, CONTRADICTS, etc.) only need to clear
// the confidence floor.
const LinkTypeSimilarTo = "SIMILAR_TO"

// LinkCandidate is one neighbor returned by the nearest-neighbor
// search. The TargetEntryID is the Cortex entry id (e.g. entry:01H...)
// retrieved from Weaviate's cortex_id property, not the raw Weaviate
// UUID. CosineSimilarity is recovered from Weaviate's distance field
// by the adapter and lives in [-1, 1].
type LinkCandidate struct {
	TargetEntryID    string
	CosineSimilarity float64
}

// LinkProposal is one link the LLM suggests adding between the source
// entry and a candidate. Confidence is the model's self-reported score
// in [0, 1]. The scorer treats malformed proposer output as "no
// proposals" — see LinkProposer below.
type LinkProposal struct {
	TargetEntryID string
	LinkType      string
	Confidence    float64
}

// LinkProposer is the narrow interface the pipeline needs from the
// LLM-backed link-scoring helper. Implementations MUST treat an
// unparseable model response as a non-error empty slice: the bead's
// AC3 requires that a malformed LLM body still allows the source entry
// to commit, and the simplest way to honor that is to swallow the
// parse failure inside the proposer so DeriveLinks does not need to
// distinguish "model unreachable" from "model returned garbage".
// Callers may also return an error; DeriveLinks will convert any error
// into an empty result.
type LinkProposer interface {
	Propose(ctx context.Context, sourceBody string, candidates []LinkCandidate) ([]LinkProposal, error)
}

// LinkDerivationConfig holds the thresholds the scorer enforces. The
// zero value is intentionally strict (nothing passes) so a caller that
// forgets to populate it will emit zero links rather than accidentally
// flooding the graph.
type LinkDerivationConfig struct {
	ConfidenceFloor    float64
	SimilarCosineFloor float64
}

// AcceptedLink pairs a proposal that cleared both floors with the
// cosine similarity that was used during the filter. The cosine is
// carried through so the emitted datom can record it for later
// auditing without re-querying Weaviate.
type AcceptedLink struct {
	Proposal LinkProposal
	Cosine   float64
}

// DeriveLinks runs the A-MEM scoring pass. It is a pure function from
// (proposer output, candidate cosines, config) → accepted set — it
// takes no log writer and no adapter, so it cannot acquire the log
// flock. AC4 holds by construction.
//
// Behavior:
//   - nil proposer, empty candidates, or a proposer error all yield an
//     empty result with no error. The caller has already committed the
//     source entry; there is nothing to roll back.
//   - Proposals whose TargetEntryID is not present in candidates are
//     dropped. An LLM that invents an id never gets to fabricate a
//     link to a random entry.
//   - Proposals below ConfidenceFloor are dropped.
//   - SIMILAR_TO proposals additionally require cosine ≥ SimilarCosineFloor.
//     Other link types only need the confidence floor.
func DeriveLinks(ctx context.Context, proposer LinkProposer, sourceBody string, candidates []LinkCandidate, cfg LinkDerivationConfig) []AcceptedLink {
	if proposer == nil || len(candidates) == 0 {
		return nil
	}
	proposals, err := proposer.Propose(ctx, sourceBody, candidates)
	if err != nil || len(proposals) == 0 {
		return nil
	}
	cosine := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		cosine[c.TargetEntryID] = c.CosineSimilarity
	}
	var accepted []AcceptedLink
	for _, p := range proposals {
		if p.TargetEntryID == "" || p.LinkType == "" {
			continue
		}
		if p.Confidence < cfg.ConfidenceFloor {
			continue
		}
		cos, ok := cosine[p.TargetEntryID]
		if !ok {
			continue
		}
		if p.LinkType == LinkTypeSimilarTo && cos < cfg.SimilarCosineFloor {
			continue
		}
		accepted = append(accepted, AcceptedLink{Proposal: p, Cosine: cos})
	}
	return accepted
}

// BuildLinkDatoms turns an accepted-link set into sealed datoms that
// the caller can append to the log as a separate transaction group.
// The attribute naming convention is "link.<TYPE>" (e.g. link.SIMILAR_TO)
// so replay and as-of queries can select link datoms by prefix.
//
// The caller supplies the outer tx/ts/actor/invocation identity; this
// function does not mint its own tx because derived links are written
// as a distinct, separately-committed group — the bead explicitly
// wants this to happen AFTER the main observe Append has returned and
// released its flock.
func BuildLinkDatoms(srcEntryID, tx, ts, actor, invocationID string, links []AcceptedLink) ([]datom.Datom, error) {
	if len(links) == 0 {
		return nil, nil
	}
	out := make([]datom.Datom, 0, len(links))
	for _, l := range links {
		payload := map[string]any{
			"target":     l.Proposal.TargetEntryID,
			"confidence": l.Proposal.Confidence,
			"cosine":     l.Cosine,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal link payload: %w", err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        actor,
			Op:           datom.OpAdd,
			E:            srcEntryID,
			A:            "link." + l.Proposal.LinkType,
			V:            raw,
			Src:          "observe",
			InvocationID: invocationID,
		}
		if err := d.Seal(); err != nil {
			return nil, fmt.Errorf("seal link datom: %w", err)
		}
		out = append(out, d)
	}
	return out, nil
}
