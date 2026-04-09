// Package datom defines the atomic unit of the Cortex log — an immutable
// (entity, attribute, value, tx, op) tuple with provenance and a SHA-256
// checksum — plus its canonical JSONL wire form.
//
// The log is append-only; all mutation is expressed as later datoms with
// higher `tx` ULIDs. A small registry of attributes are marked as
// "last-write-wins" (LWW), which changes only their *replay* semantics
// (rebuild applies only the highest-tx LWW datom per (entity, attribute)),
// never their write semantics.
package datom

// Op is the datom operation kind.
type Op string

const (
	// OpAdd asserts a new value.
	OpAdd Op = "add"
	// OpRetract retracts a prior assertion.
	OpRetract Op = "retract"
)

// AttrSpec describes the semantic properties of an attribute.
type AttrSpec struct {
	Name string
	LWW  bool
}

// Registry tracks attribute metadata. It is deliberately small in Phase 1;
// unknown attributes are still writable (the log remains authoritative) but
// do not participate in LWW collapse at replay time.
type Registry struct {
	attrs map[string]AttrSpec
}

// NewRegistry builds the default Phase 1 registry. The LWW attribute list
// matches the "Activation Datom Replay Semantics" section of cortex-spec.md:
//
//	base_activation, retrieval_count, last_retrieved_at,
//	evicted_at, evicted_at_retracted.
func NewRegistry() *Registry {
	r := &Registry{attrs: map[string]AttrSpec{}}
	lww := []string{
		"base_activation",
		"retrieval_count",
		"last_retrieved_at",
		"evicted_at",
		"evicted_at_retracted",
	}
	for _, n := range lww {
		r.attrs[n] = AttrSpec{Name: n, LWW: true}
	}
	return r
}

// Register adds or replaces an attribute spec.
func (r *Registry) Register(spec AttrSpec) { r.attrs[spec.Name] = spec }

// IsLWW reports whether the attribute uses last-write-wins replay semantics.
// Unknown attributes return false.
func (r *Registry) IsLWW(name string) bool {
	return r.attrs[name].LWW
}

// Spec returns the registered spec for an attribute, if any.
func (r *Registry) Spec(name string) (AttrSpec, bool) {
	s, ok := r.attrs[name]
	return s, ok
}
