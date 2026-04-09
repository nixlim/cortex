package datom

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func mkDatom(attr string, raw json.RawMessage) *Datom {
	return &Datom{
		Tx:           "01J000000000000000000000TX",
		Ts:           "2026-04-10T00:00:00.000000000Z",
		Actor:        "agent:test",
		Op:           OpAdd,
		E:            "entry:abc",
		A:            attr,
		V:            raw,
		Src:          "observe",
		InvocationID: "01J000000000000000000000IV",
	}
}

func TestMarshalUnmarshalRoundTripAllRegistryAttrs(t *testing.T) {
	reg := NewRegistry()
	// Add a plain (non-LWW) attribute to exercise the "both kinds" path.
	reg.Register(AttrSpec{Name: "body", LWW: false})

	cases := []struct {
		attr string
		v    json.RawMessage
	}{
		{"base_activation", json.RawMessage(`0.5`)},
		{"retrieval_count", json.RawMessage(`3`)},
		{"last_retrieved_at", json.RawMessage(`"2026-04-10T00:00:00Z"`)},
		{"evicted_at", json.RawMessage(`"2026-04-10T00:00:00Z"`)},
		{"evicted_at_retracted", json.RawMessage(`true`)},
		{"body", json.RawMessage(`"observation body with \"quotes\""`)},
	}
	for _, c := range cases {
		t.Run(c.attr, func(t *testing.T) {
			d := mkDatom(c.attr, c.v)
			line, err := Marshal(d)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			// Acceptance: terminated by a single 0x0A.
			if line[len(line)-1] != '\n' {
				t.Fatalf("missing newline terminator")
			}
			if bytes.HasSuffix(line, []byte("\n\n")) {
				t.Fatalf("double newline terminator")
			}
			round, err := Unmarshal(line)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// Re-marshal and compare byte-for-byte.
			round.Checksum = ""
			line2, err := Marshal(round)
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if !bytes.Equal(line, line2) {
				t.Errorf("byte round-trip mismatch:\nA=%s\nB=%s", line, line2)
			}
		})
	}
}

func TestChecksumTamperDetection(t *testing.T) {
	d := mkDatom("base_activation", json.RawMessage(`0.5`))
	line, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte inside the "v" value. The JSON is compact so "0.5"
	// appears once; mutate the first '5' to '6'.
	idx := bytes.Index(line, []byte(`0.5`))
	if idx < 0 {
		t.Fatal("could not locate value bytes to tamper")
	}
	tampered := append([]byte(nil), line...)
	tampered[idx+2] = '6'

	_, err = Unmarshal(tampered)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}
}

func TestRegistryIsLWW(t *testing.T) {
	reg := NewRegistry()
	lww := []string{"base_activation", "retrieval_count", "last_retrieved_at", "evicted_at", "evicted_at_retracted"}
	for _, a := range lww {
		if !reg.IsLWW(a) {
			t.Errorf("%s should be LWW", a)
		}
	}
	if reg.IsLWW("body") {
		t.Error("body must not be LWW")
	}
	if reg.IsLWW("unknown_attr") {
		t.Error("unknown attrs must default to non-LWW")
	}
}

func TestCanonicalKeyOrderStable(t *testing.T) {
	// Two datoms built with different field-assignment order must
	// produce the same bytes.
	a := &Datom{
		E: "e1", A: "a1", V: json.RawMessage(`"v"`),
		Tx: "T", Ts: "TS", Actor: "A", Op: OpAdd, Src: "observe", InvocationID: "I",
	}
	b := &Datom{
		InvocationID: "I", Src: "observe", Op: OpAdd, Actor: "A", Ts: "TS", Tx: "T",
		V: json.RawMessage(`"v"`), A: "a1", E: "e1",
	}
	la, err := Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	lb, err := Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(la, lb) {
		t.Fatalf("canonical form not stable:\nA=%s\nB=%s", la, lb)
	}
}

func TestSealIdempotent(t *testing.T) {
	d := mkDatom("base_activation", json.RawMessage(`0.5`))
	if err := d.Seal(); err != nil {
		t.Fatal(err)
	}
	first := d.Checksum
	if err := d.Seal(); err != nil {
		t.Fatal(err)
	}
	if d.Checksum != first {
		t.Errorf("Seal not idempotent: %s vs %s", first, d.Checksum)
	}
	if err := d.Verify(); err != nil {
		t.Errorf("Verify after Seal: %v", err)
	}
}
