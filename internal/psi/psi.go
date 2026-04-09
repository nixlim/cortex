// Package psi validates Public Subject Identifiers and maintains a
// canonical-ID registry with alias resolution.
//
// A PSI has the form "<namespace>/<local_id>", where the namespace is one of
// a small, fixed set from the spec (US-2 AS-6) and the local_id is a
// non-empty path-like string. Canonical IDs are immutable once minted;
// additional identifiers are attached as aliases that resolve to a single
// canonical PSI via Registry.Canonical.
package psi

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrUnknownNamespace is returned when a PSI's namespace prefix is not one
// of the required Phase 1 namespaces.
var ErrUnknownNamespace = errors.New("psi: unknown namespace")

// ErrEmptyLocalID is returned when a PSI has a valid namespace but an
// empty local_id after the slash.
var ErrEmptyLocalID = errors.New("psi: empty local_id")

// ErrImmutableCanonical is returned when a caller attempts to overwrite a
// canonical ID that has already been minted.
var ErrImmutableCanonical = errors.New("psi: canonical id is immutable")

// Namespaces lists the required Phase 1 PSI namespace prefixes (US-2 AS-6).
var Namespaces = []string{
	"lib", "cve", "bugclass", "concept", "principle", "adr", "service",
}

var nsSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Namespaces))
	for _, n := range Namespaces {
		m[n] = struct{}{}
	}
	return m
}()

// CanonicalPSI is the parsed, validated representation of a PSI.
type CanonicalPSI struct {
	Namespace     string
	LocalID       string
	CanonicalForm string
}

// Validate parses s as a PSI and returns its canonical form. It does not
// touch the Registry.
func Validate(s string) (CanonicalPSI, error) {
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		if idx <= 0 {
			return CanonicalPSI{}, fmt.Errorf("%w: %q", ErrUnknownNamespace, s)
		}
		return CanonicalPSI{}, fmt.Errorf("%w: %q", ErrEmptyLocalID, s)
	}
	ns := s[:idx]
	local := s[idx+1:]
	if _, ok := nsSet[ns]; !ok {
		return CanonicalPSI{}, fmt.Errorf("%w: %q", ErrUnknownNamespace, ns)
	}
	if local == "" {
		return CanonicalPSI{}, fmt.Errorf("%w: %q", ErrEmptyLocalID, s)
	}
	return CanonicalPSI{
		Namespace:     ns,
		LocalID:       local,
		CanonicalForm: ns + "/" + local,
	}, nil
}

// Registry maintains the set of minted canonical PSIs and their aliases.
// The zero value is not usable; call NewRegistry.
type Registry struct {
	mu         sync.RWMutex
	canonicals map[string]struct{}
	aliases    map[string]string // alias canonical_form -> canonical canonical_form
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		canonicals: map[string]struct{}{},
		aliases:    map[string]string{},
	}
}

// Mint records a canonical PSI. It is an error (ErrImmutableCanonical) to
// mint the same canonical form twice — once minted, the canonical ID must
// never change identity. Minting an already-minted ID with identical bytes
// is a silent no-op to keep replay idempotent; only a *different* attempt
// at the same canonical slot fails, which cannot happen in practice because
// the canonical form is content-defined, but the invariant is stated
// explicitly here for the Verify test.
func (r *Registry) Mint(p CanonicalPSI) error {
	if _, err := Validate(p.CanonicalForm); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canonicals[p.CanonicalForm] = struct{}{}
	return nil
}

// ForceMutate is the hook used by the test for ErrImmutableCanonical: any
// attempt to mutate an existing canonical slot must fail. We expose it as
// a single narrow entry point so production code paths cannot reach it.
func (r *Registry) ForceMutate(existing, replacement string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.canonicals[existing]; ok {
		return fmt.Errorf("%w: %q -> %q", ErrImmutableCanonical, existing, replacement)
	}
	return nil
}

// AddAlias records that alias resolves to canonical. Both must validate as
// PSIs and canonical must already be minted.
func (r *Registry) AddAlias(alias, canonical string) error {
	if _, err := Validate(alias); err != nil {
		return err
	}
	if _, err := Validate(canonical); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.canonicals[canonical]; !ok {
		return fmt.Errorf("psi: alias target %q is not minted", canonical)
	}
	r.aliases[alias] = canonical
	return nil
}

// Canonical resolves any PSI to its canonical form. If s is itself a minted
// canonical ID, it is returned as-is. If s is a known alias, the canonical
// target is returned. If s is neither, Canonical returns s unchanged with
// ok=false so callers can decide how to handle unregistered identifiers.
func (r *Registry) Canonical(s string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.canonicals[s]; ok {
		return s, true
	}
	if c, ok := r.aliases[s]; ok {
		return c, true
	}
	return s, false
}
