package datom

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrChecksumMismatch is returned by Verify and Unmarshal when the checksum
// recorded on a datom does not match a fresh recomputation over its other
// fields. Any single-byte tamper flips the checksum.
var ErrChecksumMismatch = errors.New("datom: checksum mismatch")

// canonicalOrder lists the JSON keys in the canonical serialization order.
// Keep this in lockstep with the Datom struct's json tags. The checksum is
// deliberately omitted from this list.
var canonicalOrder = []string{
	"tx", "ts", "actor", "op", "e", "a", "v", "src", "invocation_id",
}

// canonicalBody returns the canonical byte serialization of d's fields,
// excluding checksum. This is the exact input used for SHA-256.
//
// The format is a JSON object with keys in canonicalOrder, no whitespace,
// values JSON-encoded via encoding/json (so strings are escaped consistently
// and v passes through as its raw stored bytes).
func canonicalBody(d *Datom) ([]byte, error) {
	values := map[string]any{
		"tx":            d.Tx,
		"ts":            d.Ts,
		"actor":         d.Actor,
		"op":            string(d.Op),
		"e":             d.E,
		"a":             d.A,
		"src":           d.Src,
		"invocation_id": d.InvocationID,
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range canonicalOrder {
		if i > 0 {
			buf.WriteByte(',')
		}
		// Key.
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')

		// Value.
		if k == "v" {
			// RawMessage passes through unchanged. Guard against nil:
			// an empty raw message is serialized as the JSON null so
			// the checksum remains well-defined.
			if len(d.V) == 0 {
				buf.WriteString("null")
			} else {
				// Compact incoming RawMessage to eliminate incidental
				// whitespace and make the canonical form stable.
				if err := json.Compact(&buf, d.V); err != nil {
					return nil, fmt.Errorf("datom: compact v: %w", err)
				}
			}
			continue
		}
		vb, err := json.Marshal(values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// computeChecksum returns the hex SHA-256 of the datom's canonical body.
func computeChecksum(d *Datom) (string, error) {
	body, err := canonicalBody(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Seal populates d.Checksum with the computed value. Call this after
// assembling a datom and before writing it to the log.
func (d *Datom) Seal() error {
	sum, err := computeChecksum(d)
	if err != nil {
		return err
	}
	d.Checksum = sum
	return nil
}

// Verify recomputes d's checksum and returns ErrChecksumMismatch if it does
// not match the recorded one.
func (d *Datom) Verify() error {
	want, err := computeChecksum(d)
	if err != nil {
		return err
	}
	if want != d.Checksum {
		return ErrChecksumMismatch
	}
	return nil
}

// Marshal returns a single-line canonical JSONL byte slice for d, terminated
// by exactly one 0x0A. Seal() is called automatically if Checksum is empty.
func Marshal(d *Datom) ([]byte, error) {
	if d.Checksum == "" {
		if err := d.Seal(); err != nil {
			return nil, err
		}
	}
	body, err := canonicalBody(d)
	if err != nil {
		return nil, err
	}
	// Insert the checksum key/value before the closing brace of body.
	// body is guaranteed to end with '}'.
	cks, err := json.Marshal(d.Checksum)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+len(cks)+16)
	out = append(out, body[:len(body)-1]...)
	out = append(out, ',', '"', 'c', 'h', 'e', 'c', 'k', 's', 'u', 'm', '"', ':')
	out = append(out, cks...)
	out = append(out, '}', '\n')
	return out, nil
}

// Unmarshal parses a JSONL line (with or without trailing newline) into a
// Datom and verifies its checksum. A mismatch returns ErrChecksumMismatch.
func Unmarshal(line []byte) (*Datom, error) {
	// Tolerate a trailing newline; parse as JSON.
	trimmed := bytes.TrimRight(line, "\n")
	var d Datom
	if err := json.Unmarshal(trimmed, &d); err != nil {
		return nil, fmt.Errorf("datom: unmarshal: %w", err)
	}
	if err := d.Verify(); err != nil {
		return nil, err
	}
	return &d, nil
}
