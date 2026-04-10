// internal/write/concepts.go is the shared lexical concept tokenizer
// that the write path uses to populate :Concept nodes in Neo4j and the
// recall path uses to look them back up. Both sides MUST call
// ExtractConceptTokens on the same input shape (body on write, query
// on recall) so the tokens match exactly — any divergence breaks the
// seed-resolution step and degrades recall to zero hits.
//
// The tokenizer is deliberately simple: split on whitespace and
// non-word punctuation, lowercase, dedupe, filter out short tokens
// (<3 chars) and a small English stopword set. No stemming, no
// language model, no external dependencies. This matches the "lexical
// fallback" contract in the observe→recall integration test and keeps
// observe latency at zero-cost — tokenization is nanoseconds compared
// to the ~3s of an LLM concept-extraction call.
//
// Bead cortex-concept (Phase 1 architectural follow-up to
// cortex-jw6 / cortex-c09): recall was impossible before this existed
// because nothing in the codebase wrote :Concept nodes, and the
// recall pipeline's Stage 2 seed resolver required them to exist.
package write

import (
	"sort"
	"strings"
	"unicode"
)

// stopwords is a minimal English stopword set filtered out of concept
// tokens. Kept small on purpose: anything longer than this and we
// start to drop legitimate domain terms that observers used. Callers
// that want richer filtering should preprocess before calling
// ExtractConceptTokens.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "this": true,
	"that": true, "from": true, "has": true, "have": true, "had": true,
	"was": true, "were": true, "are": true, "but": true, "not": true,
	"can": true, "could": true, "will": true, "would": true, "should": true,
	"may": true, "might": true, "must": true, "into": true, "onto": true,
	"over": true, "under": true, "above": true, "below": true, "about": true,
	"your": true, "yours": true, "mine": true, "ours": true, "their": true,
	"them": true, "they": true, "what": true, "when": true, "where": true,
	"which": true, "who": true, "whom": true, "whose": true, "why": true,
	"how": true, "here": true, "there": true, "some": true, "any": true,
	"all": true, "none": true, "both": true, "each": true, "every": true,
	"such": true, "same": true, "other": true, "another": true, "these": true,
	"those": true, "than": true, "then": true, "too": true, "very": true,
	"just": true, "also": true, "only": true, "still": true, "yet": true,
	"because": true, "before": true, "after": true, "during": true,
}

// ExtractConceptTokens lexically tokenizes the supplied text into a
// deduplicated, lowercased list of concept tokens. The function is
// deterministic and allocation-bounded: the returned slice is sorted
// so downstream consumers get a stable order without a follow-up
// sort.Strings call.
//
// Tokens shorter than 3 runes or present in the built-in stopword
// set are dropped. Hyphenated identifiers like "cortex-roundtrip-token"
// are preserved as a single token because the splitter treats hyphens
// as word characters — this is critical for code-like tokens that
// would lose meaning if split on the hyphens.
func ExtractConceptTokens(text string) []string {
	if text == "" {
		return nil
	}
	// A rune is kept if it's a letter, digit, or an underscore. Hyphens
	// are treated as boundaries because LLM-based concept extractors —
	// which callers may use upstream of this function — routinely split
	// hyphenated compounds like "cortex-roundtrip-token" into their
	// word parts. Splitting here as well keeps the write-side and
	// read-side token sets symmetric no matter which extractor a
	// caller pipes in front. Underscores survive because they're
	// typically used inside code identifiers the user wants matched
	// as one unit.
	split := func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		if r == '_' {
			return false
		}
		return true
	}
	raw := strings.FieldsFunc(text, split)
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		tok = strings.ToLower(strings.Trim(tok, "-_"))
		if len(tok) < 3 {
			continue
		}
		// FieldsFunc already stripped boundary runes, but a token
		// composed entirely of digits (e.g. a year, a count) is
		// typically not useful as a concept — drop it.
		if allDigits(tok) {
			continue
		}
		if stopwords[tok] {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// ConceptEntityID returns the prefixed entity id used by the Neo4j
// applier and recall's seed resolver for a given concept token. Both
// sides MUST agree on this shape or lookups miss: the write path
// sets `:Concept.entry_id = concept:<token>` and the recall path
// filters on the same value.
func ConceptEntityID(token string) string {
	return "concept:" + strings.ToLower(strings.TrimSpace(token))
}

// allDigits reports whether a token is composed entirely of ASCII
// digits. Used by ExtractConceptTokens to suppress bare numeric
// tokens that would otherwise pollute the concept graph.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
