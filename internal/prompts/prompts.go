// Package prompts provides the structured prompt template library for all
// LLM call sites in Cortex.
//
// Every template embeds user-provided content strictly inside an explicit
// "<<<USER_CONTENT>>> ... <<<END_USER_CONTENT>>>" block. The package
// sanitizes that content so the delimiter markers themselves cannot appear
// verbatim inside the delimited region — this is the only line of defense
// against a user payload that tries to close the block and inject
// instructions into the surrounding prompt region.
//
// There is exactly one way to render a template: Render(name, Data). Call
// sites never reach for text/template directly and never format user input
// into prompt strings with fmt.Sprintf.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed tmpl/*.tmpl
var tmplFS embed.FS

const (
	// OpenDelim marks the start of a user-content block.
	OpenDelim = "<<<USER_CONTENT>>>"
	// CloseDelim marks the end of a user-content block.
	CloseDelim = "<<<END_USER_CONTENT>>>"
)

// Data is the per-render input. All fields are sanitized before being
// substituted into the template so that the close delimiter cannot be
// emitted verbatim inside the delimited region.
type Data struct {
	Body       string
	Candidates string
}

// Template names. Consumers MUST use these constants, not string literals.
const (
	NameConceptExtraction = "concept_extraction"
	NameTrailSummary      = "trail_summary"
	NameLinkDerivation    = "link_derivation"
	NameFrameProposal     = "frame_proposal"
	NameCommunitySummary  = "community_summary"
	NameModuleSummary     = "module_summary"
)

// ModuleSummarySchema is the JSON schema Ollama enforces when the
// ingest summarizer calls GenerateStructured with the module_summary
// prompt. The five fields are all required so the summarizer never
// has to handle a partially-populated object; empty arrays are valid.
//
// The schema is stored as a raw JSON message rather than a Go struct
// so it can be passed through to the Ollama /api/generate "format"
// field verbatim. Keep the schema colocated with the template — both
// describe the module_summary contract.
var ModuleSummarySchema = []byte(`{
  "type": "object",
  "properties": {
    "summary":      { "type": "string" },
    "identifiers":  { "type": "array", "items": { "type": "string" } },
    "algorithms":   { "type": "array", "items": { "type": "string" } },
    "dependencies": { "type": "array", "items": { "type": "string" } },
    "searchable":   { "type": "array", "items": { "type": "string" } }
  },
  "required": ["summary","identifiers","algorithms","dependencies","searchable"]
}`)

var all = []string{
	NameConceptExtraction,
	NameTrailSummary,
	NameLinkDerivation,
	NameFrameProposal,
	NameCommunitySummary,
	NameModuleSummary,
}

// All returns the registered template names in stable order.
func All() []string {
	out := make([]string, len(all))
	copy(out, all)
	return out
}

var parsed = func() map[string]*template.Template {
	m := make(map[string]*template.Template, len(all))
	for _, n := range all {
		b, err := tmplFS.ReadFile("tmpl/" + n + ".tmpl")
		if err != nil {
			panic(fmt.Sprintf("prompts: missing embedded template %q: %v", n, err))
		}
		t, err := template.New(n).Parse(string(b))
		if err != nil {
			panic(fmt.Sprintf("prompts: parse %q: %v", n, err))
		}
		m[n] = t
	}
	return m
}()

// Sanitize makes s safe to place inside a USER_CONTENT block. It rewraps any
// occurrence of the close delimiter so the delimiter markers remain
// unambiguous. Triple-dashes ("---") that might be mistaken for a markdown
// section break are also rewrapped — reflection prompts historically use
// "---" as a secondary fence, and a user body containing "---" could still
// steer downstream tooling.
//
// The rewrap inserts a zero-width marker between the first two characters
// of each delimiter-like substring, producing a form that no longer matches
// the sentinel but is still visually recognizable in logs.
func Sanitize(s string) string {
	// Handle close delimiter first (most dangerous).
	s = strings.ReplaceAll(s, CloseDelim, "<<\u200B<END_USER_CONTENT>>>")
	// Handle open delimiter symmetrically so a downstream parser can never
	// be fooled into thinking a new block started.
	s = strings.ReplaceAll(s, OpenDelim, "<<\u200B<USER_CONTENT>>>")
	// Rewrap bare triple-dash fences.
	s = strings.ReplaceAll(s, "---", "--\u200B-")
	return s
}

// Render executes the named template with the provided data. User-supplied
// strings are sanitized before substitution.
func Render(name string, d Data) (string, error) {
	t, ok := parsed[name]
	if !ok {
		return "", fmt.Errorf("prompts: unknown template %q", name)
	}
	clean := Data{
		Body:       Sanitize(d.Body),
		Candidates: Sanitize(d.Candidates),
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, clean); err != nil {
		return "", fmt.Errorf("prompts: render %q: %w", name, err)
	}
	return buf.String(), nil
}
