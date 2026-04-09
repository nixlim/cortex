package datom

import "encoding/json"

// Datom is one immutable fact in the Cortex append-only log.
//
// Field order below defines the canonical JSON key order used by Marshal.
// The checksum field is computed over the canonical serialization of the
// other fields (see codec.go) and is excluded from that computation.
type Datom struct {
	// Tx is the transaction ULID that groups one or more datoms written
	// together. It is the authoritative causal ordering key.
	Tx string `json:"tx"`

	// Ts is the wall-clock timestamp (RFC 3339 nanosecond precision) at
	// which the writer prepared the datom. Not authoritative for ordering;
	// Tx is.
	Ts string `json:"ts"`

	// Actor is the invoking agent or user identity.
	Actor string `json:"actor"`

	// Op is the datom operation kind (add|retract).
	Op Op `json:"op"`

	// E is the entity ID the datom is asserting about.
	E string `json:"e"`

	// A is the attribute name.
	A string `json:"a"`

	// V is the attribute value. json.RawMessage preserves the exact bytes
	// the caller supplied so value round-trips are byte-identical across
	// Marshal/Unmarshal, even for numeric types that a generic any would
	// lose precision on.
	V json.RawMessage `json:"v"`

	// Src names the write path that produced the datom (e.g. "observe",
	// "ingest", "reflect"). Used for provenance and debugging.
	Src string `json:"src"`

	// InvocationID is the ULID generated at the start of the enclosing
	// cortex command invocation. All datoms and ops.log entries from the
	// same command share this value.
	InvocationID string `json:"invocation_id"`

	// Checksum is the hex-encoded SHA-256 of the canonical serialization
	// of all other fields. It is excluded from that serialization so
	// verifying a datom is deterministic.
	Checksum string `json:"checksum"`
}
