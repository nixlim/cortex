package write

import (
	"reflect"
	"testing"
)

func TestExtractConceptTokens_LexicalSplit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty string returns nil",
			in:   "",
			want: nil,
		},
		{
			name: "hyphenated identifier split into parts",
			in:   "cortex-roundtrip-token",
			want: []string{"cortex", "roundtrip", "token"},
		},
		{
			name: "underscored identifier preserved as single token",
			in:   "payment_gateway",
			want: []string{"payment_gateway"},
		},
		{
			name: "full sentence tokenized + stopwords removed + sorted",
			in:   "round-3 regression entry: cortex-roundtrip-token must come back via observe-recall",
			want: []string{"back", "come", "cortex", "entry", "observe", "recall", "regression", "round", "roundtrip", "token", "via"},
		},
		{
			name: "stopwords stripped",
			in:   "this is the canonical example and that was the other",
			want: []string{"canonical", "example"},
		},
		{
			name: "bare digits dropped, alphanumeric kept",
			in:   "123 404 payment_gateway v2 2025",
			want: []string{"payment_gateway"},
		},
		{
			name: "case folded + duplicates merged",
			in:   "Cortex cortex CORTEX database DATABASE",
			want: []string{"cortex", "database"},
		},
		{
			name: "punctuation boundaries",
			in:   "foo.bar/baz;qux",
			want: []string{"bar", "baz", "foo", "qux"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractConceptTokens(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractConceptTokens(%q)\n  got  %v\n  want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestConceptEntityID(t *testing.T) {
	if got := ConceptEntityID("Cortex"); got != "concept:cortex" {
		t.Errorf("ConceptEntityID(\"Cortex\") = %q, want concept:cortex", got)
	}
	if got := ConceptEntityID("  cortex-roundtrip-token  "); got != "concept:cortex-roundtrip-token" {
		t.Errorf("ConceptEntityID(padded) = %q, want concept:cortex-roundtrip-token", got)
	}
}

// TestConceptSymmetry proves the write-side and read-side callers
// produce identical concept ids for a shared token, which is the
// invariant the seed resolver depends on.
func TestConceptSymmetry(t *testing.T) {
	body := "An entry about cortex-roundtrip-token and its observe→recall path."
	query := "cortex-roundtrip-token"

	bodyTokens := ExtractConceptTokens(body)
	queryTokens := ExtractConceptTokens(query)

	bodyIDs := make(map[string]bool)
	for _, tok := range bodyTokens {
		bodyIDs[ConceptEntityID(tok)] = true
	}
	for _, tok := range queryTokens {
		id := ConceptEntityID(tok)
		if !bodyIDs[id] {
			t.Errorf("query token %q -> id %q not present in body ids %v",
				tok, id, bodyTokens)
		}
	}
}
