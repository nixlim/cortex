// Trail read side: materializes trail manifests and lists by walking
// the segmented datom log via internal/log.ReadAll. The functions here
// are deliberately simple linear scans because Phase 1 corpora fit in
// memory; richer indexes can be layered on later without changing the
// public surface.
package trail

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
)

// EntryRef is one observation that belongs to a trail. Tx is preserved
// so callers that need a stable ordering key (history, replay) can rely
// on it; the slice returned by Load is already sorted by Tx ascending
// (the order observe wrote the entries) which is what AC3 of the spec
// calls "IN_TRAIL.order".
type EntryRef struct {
	ID string `json:"id"`
	Tx string `json:"tx"`
}

// Manifest is the fully materialized form of one trail. Optional fields
// are empty strings rather than pointers so the JSON shape is flat.
type Manifest struct {
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	Agent     string     `json:"agent,omitempty"`
	StartedAt string     `json:"started_at,omitempty"`
	EndedAt   string     `json:"ended_at,omitempty"`
	Summary   string     `json:"summary,omitempty"`
	Entries   []EntryRef `json:"entries"`
}

// Summary is the row shape returned by List. EntryCount avoids forcing
// the caller to materialize every entry just to count them.
type Summary struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Agent      string `json:"agent,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	EntryCount int    `json:"entry_count"`
}

// Load reads every healthy segment in segments and reconstructs one
// trail Manifest. ErrTrailNotFound is returned when no datoms with
// E=trailID exist. Entry refs come back in tx-ULID-ascending order so
// callers see them in the order observe originally appended them.
func Load(segments []string, trailID string) (*Manifest, error) {
	if trailID == "" {
		return nil, errs.Validation("MISSING_TRAIL_ID",
			"trail.Load requires a non-empty trail id", nil)
	}

	datoms, err := log.ReadAll(segments)
	if err != nil {
		return nil, errs.Operational("LOG_READ_FAILED",
			"could not read log segments", err)
	}

	m := &Manifest{ID: trailID}
	saw := false
	for i := range datoms {
		d := &datoms[i]
		switch {
		case d.E == trailID:
			saw = true
			if err := applyAttribute(m, d); err != nil {
				return nil, err
			}
		case d.A == AttrTrail:
			// Entry-side reference; check whether it points at our trail.
			var ref string
			if err := json.Unmarshal(d.V, &ref); err == nil && ref == trailID {
				m.Entries = append(m.Entries, EntryRef{ID: d.E, Tx: d.Tx})
			}
		}
	}
	if !saw {
		return nil, ErrTrailNotFound
	}
	return m, nil
}

// List enumerates every Trail entity in segments. Results are sorted
// by EndedAt descending (then StartedAt descending) so the most
// recently completed trails surface first; in-progress trails (no
// EndedAt) sort before all closed trails because the empty string
// compares less than any RFC3339 timestamp — which is the wrong sort
// for our purposes — so the comparator promotes empty EndedAt to a
// high sentinel.
func List(segments []string) ([]Summary, error) {
	datoms, err := log.ReadAll(segments)
	if err != nil {
		return nil, errs.Operational("LOG_READ_FAILED",
			"could not read log segments", err)
	}

	byID := make(map[string]*Summary)
	get := func(id string) *Summary {
		s, ok := byID[id]
		if !ok {
			s = &Summary{ID: id}
			byID[id] = s
		}
		return s
	}

	for i := range datoms {
		d := &datoms[i]
		if strings.HasPrefix(d.E, EntityPrefix) {
			s := get(d.E)
			if err := applySummaryAttribute(s, d); err != nil {
				return nil, err
			}
			continue
		}
		if d.A == AttrTrail {
			var ref string
			if err := json.Unmarshal(d.V, &ref); err == nil && strings.HasPrefix(ref, EntityPrefix) {
				get(ref).EntryCount++
			}
		}
	}

	out := make([]Summary, 0, len(byID))
	for _, s := range byID {
		// A Trail entity is one whose kind datom said "Trail". The map
		// may contain spurious IDs from entry datoms whose ref happened
		// to start with the prefix; filter those out by requiring at
		// least one non-zero metadata field.
		if s.Name == "" && s.Agent == "" && s.StartedAt == "" && s.EndedAt == "" && s.EntryCount == 0 {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		ei, ej := sortableEnd(out[i].EndedAt), sortableEnd(out[j].EndedAt)
		if ei != ej {
			return ei > ej
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return out, nil
}

// sortableEnd returns a sort key that pushes empty (in-progress)
// trails ahead of closed ones in a descending sort.
func sortableEnd(s string) string {
	if s == "" {
		// "~" is greater than any digit/letter that appears in
		// RFC3339, ensuring in-progress trails come first.
		return "~"
	}
	return s
}

// ErrTrailNotFound is returned by Load when no datoms exist for the
// requested trail id. Wrapped in a Validation error at the CLI layer
// so the operator gets exit 2 with TRAIL_NOT_FOUND.
var ErrTrailNotFound = errs.Validation("TRAIL_NOT_FOUND",
	"no trail found for the requested id", nil)

// applyAttribute folds one trail-entity datom into the running manifest.
func applyAttribute(m *Manifest, d *datom.Datom) error {
	switch d.A {
	case AttrName:
		return unmarshalString(d.V, &m.Name, d.A)
	case AttrAgent:
		return unmarshalString(d.V, &m.Agent, d.A)
	case AttrStartedAt:
		return unmarshalString(d.V, &m.StartedAt, d.A)
	case AttrEndedAt:
		return unmarshalString(d.V, &m.EndedAt, d.A)
	case AttrSummary:
		return unmarshalString(d.V, &m.Summary, d.A)
	}
	// Unknown attributes are ignored — forward compatibility with
	// future trail attributes (e.g. tags).
	return nil
}

func applySummaryAttribute(s *Summary, d *datom.Datom) error {
	switch d.A {
	case AttrName:
		return unmarshalString(d.V, &s.Name, d.A)
	case AttrAgent:
		return unmarshalString(d.V, &s.Agent, d.A)
	case AttrStartedAt:
		return unmarshalString(d.V, &s.StartedAt, d.A)
	case AttrEndedAt:
		return unmarshalString(d.V, &s.EndedAt, d.A)
	}
	return nil
}

func unmarshalString(raw json.RawMessage, dst *string, attr string) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("trail: decode %s: %w", attr, err)
	}
	return nil
}
