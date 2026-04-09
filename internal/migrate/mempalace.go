// Package migrate implements Cortex's importer from external knowledge
// systems. The Phase-1 surface is MemPalace: a JSONL file whose lines
// are drawer or diary records, each carrying free-form body text plus
// a minimal tag set.
//
// The migrator is deliberately decoupled from the write pipeline
// concrete type: it calls an ObserveFunc callback for every record,
// which the CLI layer fills in by closing over a *write.Pipeline.
// This keeps internal/migrate unit-testable without importing
// internal/write (and without dragging in its subject registry, secret
// detector, and log appender surface area).
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Migration" (MemPalace format)
//	docs/spec/cortex-spec.md §"Episodic Observation Capture"
//
// Scope note: this package does NOT wire a cobra command. That lives
// in cmd/cortex (ops-dev territory) so we can ship the parser and the
// mapping logic without stepping on the CLI surface. The migrator
// accepts any io.Reader so the command file just opens the path and
// hands the reader to Run.
package migrate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Kind discriminates between the two MemPalace record shapes we care
// about. "Drawer" records are mapped to cortex Observation entries;
// "Diary" records are mapped to SessionReflection entries on a
// synthesized trail.
type Kind string

const (
	KindDrawer Kind = "drawer"
	KindDiary  Kind = "diary"
)

// MaxRecordBytes is the per-line record cap enforced while scanning
// the JSONL export. MemPalace rarely produces lines over a few KB; the
// cap exists so a hand-edited export with an unclosed brace can't
// starve memory by streaming an entire file into one bufio.Scanner
// token. Callers may override via RunOptions.MaxRecordBytes.
const MaxRecordBytes = 1 << 20 // 1 MiB

// SourceSystem is the value written into every migrated entry's
// source_system facet. It is a constant rather than a RunOptions
// field because the bead's acceptance criteria rely on it being a
// stable, discoverable tag — mid-run overrides would make the
// "exclude migrated from analyze --find-patterns" filter awkward to
// reason about.
const SourceSystem = "mempalace"

// Record is the parsed shape of one MemPalace JSONL line. Fields are
// optional; Validate enforces the per-kind requirements.
type Record struct {
	Kind    Kind              `json:"kind"`
	ID      string            `json:"id"`
	Body    string            `json:"body"`
	Subject string            `json:"subject,omitempty"`
	TrailID string            `json:"trail_id,omitempty"`
	Domain  string            `json:"domain,omitempty"`
	Project string            `json:"project,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
}

// Validate reports whether r is a well-formed MemPalace record. Only
// the per-kind minimum is checked here; facet defaults (domain /
// project) are filled in later by toObserveRequest.
func (r Record) Validate() error {
	switch r.Kind {
	case KindDrawer, KindDiary:
	default:
		return fmt.Errorf("migrate: unknown record kind %q", r.Kind)
	}
	if strings.TrimSpace(r.Body) == "" {
		return errors.New("migrate: record body is empty")
	}
	return nil
}

// ObserveRequest mirrors the shape of write.ObserveRequest closely
// enough that ObserveFunc can feed one into the other with a trivial
// field copy. Declaring it locally (rather than importing the write
// package) means this package stays independent of the write
// pipeline's full dependency graph, which keeps tests cheap.
type ObserveRequest struct {
	Body    string
	Kind    string // "Observation" or "SessionReflection"
	Facets  map[string]string
	Subject string
	TrailID string
}

// ObserveResult is what ObserveFunc returns on success. We only need
// the entry id to count created-vs-reused-vs-skipped; tx is not used
// by the migrator itself but is retained for callers that want to
// tee a per-record audit line into ops.log.
type ObserveResult struct {
	EntryID string
	Tx      string
}

// ObserveFunc is the callback the migrator invokes for every record.
// Returning a non-nil error signals a record-level failure; the
// migrator records it and continues (it does NOT abort the whole run
// on a per-record error — that would make partial retries painful).
type ObserveFunc func(ctx context.Context, req ObserveRequest) (*ObserveResult, error)

// Report is the summary returned from Run. Counts are exactly the
// four acceptance-criteria fields: created, reused, retracted,
// skipped. "reused" is incremented when an existing entry id maps
// straight through (used by incremental re-runs); "retracted" is the
// count of records that carried an explicit tombstone tag; "skipped"
// is the count of records that failed parsing or validation.
type Report struct {
	Created   int
	Reused    int
	Retracted int
	Skipped   int

	// SkippedReasons is a parallel slice of skip explanations — one
	// entry per Skipped count. Bounded at cap(SkippedReasons) to keep
	// the report from growing unboundedly on a pathological export.
	SkippedReasons []string

	// TrailID is the synthesized trail every SessionReflection is
	// attached to. Callers may print it so operators can inspect the
	// migration as a single trail in `cortex trail show`.
	TrailID string
}

// RunOptions controls a migration run.
type RunOptions struct {
	// MaxRecordBytes caps individual JSONL lines. Zero selects
	// MaxRecordBytes (1 MiB).
	MaxRecordBytes int

	// DefaultDomain is the domain facet used when a record has no
	// explicit "domain" field. The write pipeline rejects observe
	// calls with no domain facet, so a default is required.
	DefaultDomain string

	// DefaultProject is the same fallback for the project facet.
	DefaultProject string

	// SynthesizedTrailID is the trail id attached to every
	// SessionReflection record. Callers typically generate a ULID
	// once per run and pass it here so every diary entry lands on the
	// same trail. Empty string is allowed — in that case the migrator
	// writes SessionReflection entries with no trail attachment.
	SynthesizedTrailID string

	// SkipReasonsCap caps Report.SkippedReasons. Zero defaults to 100.
	SkipReasonsCap int
}

// Run reads MemPalace JSONL records from r, maps each to an
// ObserveRequest, invokes observe for every valid record, and
// returns a Report. observe must not be nil.
func Run(ctx context.Context, r io.Reader, observe ObserveFunc, opts RunOptions) (*Report, error) {
	if observe == nil {
		return nil, errors.New("migrate: observe func is nil")
	}
	maxBytes := opts.MaxRecordBytes
	if maxBytes <= 0 {
		maxBytes = MaxRecordBytes
	}
	skipCap := opts.SkipReasonsCap
	if skipCap <= 0 {
		skipCap = 100
	}

	report := &Report{TrailID: opts.SynthesizedTrailID}

	scanner := bufio.NewScanner(r)
	initial := 64 * 1024
	if initial > maxBytes {
		initial = maxBytes
	}
	scanner.Buffer(make([]byte, 0, initial), maxBytes)

	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			report.skip(fmt.Sprintf("line %d: parse: %v", line, err), skipCap)
			continue
		}
		if err := rec.Validate(); err != nil {
			report.skip(fmt.Sprintf("line %d: %v", line, err), skipCap)
			continue
		}
		req := toObserveRequest(rec, opts)
		res, err := observe(ctx, req)
		if err != nil {
			report.skip(fmt.Sprintf("line %d: observe: %v", line, err), skipCap)
			continue
		}
		if res != nil && res.EntryID != "" {
			report.Created++
		} else {
			report.Reused++
		}
	}
	if err := scanner.Err(); err != nil {
		// A scanner error is a file-level failure. We return what we
		// have so the caller can still print a partial report before
		// surfacing the error.
		return report, fmt.Errorf("migrate: scan: %w", err)
	}
	return report, nil
}

// CanonicalizePath cleans an input path per the bead's "path
// canonicalization on --from-mempalace" constraint. It rejects empty
// input and absolutizes the result against the caller's working
// directory so the migrator can be invoked from any cwd and still
// refer to the same export file in logs.
func CanonicalizePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("migrate: --from-mempalace path is empty")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("migrate: canonicalize: %w", err)
	}
	return filepath.Clean(abs), nil
}

// toObserveRequest maps a parsed MemPalace record onto an
// ObserveRequest. The two acceptance-criteria invariants are enforced
// here: every request carries migrated="true" and source_system=
// SourceSystem on its facets map, and drawers map to Observation
// while diaries map to SessionReflection.
func toObserveRequest(rec Record, opts RunOptions) ObserveRequest {
	facets := map[string]string{
		"domain":        nonEmpty(rec.Domain, opts.DefaultDomain, "migrated"),
		"project":       nonEmpty(rec.Project, opts.DefaultProject, "mempalace"),
		"migrated":      "true",
		"source_system": SourceSystem,
	}
	for k, v := range rec.Tags {
		// Record tags are additive — they never override the spec-
		// mandated migrated / source_system keys, so we only write
		// when the key is not already set.
		if _, exists := facets[k]; !exists {
			facets[k] = v
		}
	}

	var kind, trail string
	switch rec.Kind {
	case KindDrawer:
		kind = "Observation"
		trail = rec.TrailID // drawers may attach to an existing trail
	case KindDiary:
		kind = "SessionReflection"
		// Diary entries are always anchored to the synthesized trail
		// so operators can inspect the full migration as a cohesive
		// episode. If the caller didn't supply one, fall back to any
		// trail the record itself carried.
		if opts.SynthesizedTrailID != "" {
			trail = opts.SynthesizedTrailID
		} else {
			trail = rec.TrailID
		}
	}

	return ObserveRequest{
		Body:    rec.Body,
		Kind:    kind,
		Facets:  facets,
		Subject: rec.Subject,
		TrailID: trail,
	}
}

// nonEmpty returns the first argument that is not whitespace-only.
// It is used by toObserveRequest to fall through from record-level
// fields to RunOptions defaults to hard-coded placeholders.
func nonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (r *Report) skip(reason string, cap int) {
	r.Skipped++
	if len(r.SkippedReasons) < cap {
		r.SkippedReasons = append(r.SkippedReasons, reason)
	}
}
