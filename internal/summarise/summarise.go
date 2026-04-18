// Package summarise implements the continuous categorised-context
// pass (bead cortex-8sr, Pass A of the two-pass architecture in
// cortex-z33 / 8sr / jr9).
//
// For each Leiden community passed in, the Stage:
//   1. Hashes the community's membership. If the hash matches the
//      prior CommunityBrief's membership_hash, the community is
//      skipped — idle log = zero LLM calls.
//   2. Otherwise, calls Claude Code CLI in reasoning-only mode with
//      the cluster's observations and the CommunityBrief JSON schema,
//      and parses the returned structured_output as a new Frame.
//
// After all community passes complete, one final Claude call stitches
// the current set of CommunityBriefs (skipped + newly written) into a
// single ProjectBrief.
//
// The package is deliberately I/O-light: it produces Frame structs
// but does not write datoms, does not read Neo4j, does not compute
// communities. Those are the analyze pipeline's jobs (layer 3,
// cortex-8sr wire-up). Separating the two keeps summarise unit-
// testable with a fake claudecli.Runner and a handful of fixture
// Community inputs.
package summarise

import "time"

// CommunityID is a stable identifier for a Leiden community in the
// cortex knowledge graph. The summariser does not interpret it — it
// simply round-trips the value through the CommunityBrief's
// community_id slot so recall can correlate the brief to the
// :Community node in Neo4j.
type CommunityID string

// Entry is one observation the summariser feeds into the LLM call
// for a community. Body is the raw prose of the observation. The
// fields mirror what `cortex observe` writes and what the analyze
// pipeline already materialises per cluster.
type Entry struct {
	ID   string
	Kind string // Observation | SessionReflection | ObservedRace | ...
	Body string
	TS   time.Time // when the observation was written
}

// Community is the summariser's input for one cluster. EntryIDs MUST
// be present and sorted (the summariser does not sort internally —
// callers are expected to produce a canonical order so the
// membership hash is stable across runs). Entries is the full prose
// each entry carries; it parallels EntryIDs 1:1 in the same order.
type Community struct {
	ID       CommunityID
	EntryIDs []string
	Entries  []Entry
}

// PriorBrief captures just enough of a previously-written
// CommunityBrief to let the summariser decide whether to skip this
// run. The caller (layer 3) loads these from Neo4j before invoking
// the stage and indexes by community id.
type PriorBrief struct {
	CommunityID    CommunityID
	MembershipHash string // hex SHA-256 from the prior Brief's slots.membership_hash
	FrameID        string // round-tripped so the stitch can reference prior frames
}

// Frame is the summariser's output shape — a pared-down mirror of
// internal/reflect.Frame and internal/analyze.Frame. Layer 3
// converts to whichever of those the write path expects. We avoid
// importing reflect/analyze here to keep the summariser independent
// and easy to test.
type Frame struct {
	Type          string // "CommunityBrief" | "ProjectBrief"
	Slots         map[string]any
	Exemplars     []string // entry ids the brief DERIVED_FROM
	SchemaVersion string
}

// Status labels what the stage did with one community this run.
type Status string

const (
	StatusSummarised Status = "summarised" // fresh LLM call produced a new CommunityBrief
	StatusSkipped    Status = "skipped"    // membership hash matched prior — no call
	StatusFailed     Status = "failed"     // LLM call failed; prior brief (if any) retained
)

// CommunityResult reports the outcome for one community. A failure
// is isolated to the community in question; the stage as a whole
// continues to the next and still attempts the project-level stitch.
type CommunityResult struct {
	CommunityID    CommunityID
	Status         Status
	Frame          *Frame // nil when Status != StatusSummarised
	Err            error  // populated only when Status == StatusFailed
	DurationMS     int64
	MembershipHash string // always populated: the hash this run computed
}

// Report is what the stage returns after processing all communities
// and attempting the stitch. Frames is the full list of NEW frames
// (community briefs + the project brief if the stitch succeeded) in
// write order; the caller appends them to the datom log.
type Report struct {
	Frames       []Frame
	Communities  []CommunityResult
	ProjectBrief *Frame // nil when the stitch failed or was skipped
	StitchErr    error  // populated when the stitch failed
}
