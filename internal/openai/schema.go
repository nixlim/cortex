package openai

import (
	"encoding/json"
	"fmt"
	"sort"
)

// normalizeSchema rewrites a JSON schema to satisfy OpenAI's strict
// mode constraints. The strict-mode rules are:
//
//  1. Every object schema MUST set additionalProperties:false.
//  2. Every object schema's "required" array MUST list every key in
//     "properties" — strict mode does not accept optional fields.
//  3. Nullable types are NOT supported. Both `"type": "null"` and
//     `"type": ["string", "null"]` are rejected with an error that
//     points at the offending path.
//
// Cortex's prompt schemas are written against Ollama's looser
// format= contract (no additionalProperties requirement, optional
// fields allowed), so they will almost always need the first two
// rules applied. Rejecting null types is deliberate: we'd rather
// fail loudly than silently drop a field by translating it to
// "string with empty sentinel".
//
// The walk recurses into properties, items (array element schemas),
// $defs / definitions (shared subschemas), and anyOf / oneOf / allOf
// combinators. Anything else is left untouched so unknown keywords
// (descriptions, examples, titles) pass through intact.
//
// The root schema MUST itself be an object schema. OpenAI strict
// mode always wraps results in an object; a scalar or array root
// would be rejected by the API anyway, so we catch it early.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("openai: empty schema")
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("openai: decode schema: %w", err)
	}
	rootMap, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openai: root schema must be a JSON object")
	}
	if t, _ := rootMap["type"].(string); t != "" && t != "object" {
		return nil, fmt.Errorf("openai: root schema must be type=object, got %q", t)
	}
	// Force type=object on the root even when absent; strict mode
	// requires it and callers sometimes rely on Ollama's looser
	// inference.
	if _, present := rootMap["type"]; !present {
		rootMap["type"] = "object"
	}
	if err := walk(rootMap, "$"); err != nil {
		return nil, err
	}
	out, err := json.Marshal(rootMap)
	if err != nil {
		return nil, fmt.Errorf("openai: re-marshal schema: %w", err)
	}
	return out, nil
}

// walk recursively rewrites schema nodes in place. path is a
// JSON-pointer-ish breadcrumb used only for error messages.
func walk(node map[string]any, path string) error {
	if err := checkTypeForNull(node, path); err != nil {
		return err
	}

	// Objects: enforce additionalProperties:false and a required
	// array listing every property. We only touch object schemas —
	// leaf types (string, number, boolean, integer, array) don't get
	// these rules.
	if isObjectType(node) {
		if _, ok := node["additionalProperties"]; !ok {
			node["additionalProperties"] = false
		}
		if props, ok := node["properties"].(map[string]any); ok {
			// Build required from the full property set, sorted for
			// deterministic output (tests rely on the order).
			keys := make([]string, 0, len(props))
			for k := range props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			node["required"] = toAnySlice(keys)

			// Recurse into each property subschema.
			for k, v := range props {
				if sub, ok := v.(map[string]any); ok {
					if err := walk(sub, path+".properties."+k); err != nil {
						return err
					}
				}
			}
		}
	}

	// Arrays: recurse into items. OpenAI currently only supports a
	// single items schema, not a tuple, so we only handle the
	// object case.
	if items, ok := node["items"].(map[string]any); ok {
		if err := walk(items, path+".items"); err != nil {
			return err
		}
	}

	// Shared subschemas: $defs (2020-12) and definitions (draft-07).
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := node[key].(map[string]any); ok {
			for k, v := range defs {
				if sub, ok := v.(map[string]any); ok {
					if err := walk(sub, path+"."+key+"."+k); err != nil {
						return err
					}
				}
			}
		}
	}

	// Combinators: walk every branch. anyOf/oneOf/allOf carry arrays
	// of subschemas.
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := node[key].([]any); ok {
			for i, v := range arr {
				if sub, ok := v.(map[string]any); ok {
					if err := walk(sub, fmt.Sprintf("%s.%s[%d]", path, key, i)); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// checkTypeForNull rejects both the scalar "null" type and the
// array form ["string", "null"] that Cortex schemas sometimes use
// to mark a field as nullable. Strict mode does not accept either.
func checkTypeForNull(node map[string]any, path string) error {
	t, present := node["type"]
	if !present {
		return nil
	}
	switch v := t.(type) {
	case string:
		if v == "null" {
			return fmt.Errorf("openai: strict mode does not support null types at %s", path)
		}
	case []any:
		for _, elem := range v {
			if s, ok := elem.(string); ok && s == "null" {
				return fmt.Errorf("openai: strict mode does not support null types at %s", path)
			}
		}
	}
	return nil
}

// isObjectType returns true when the schema declares type=object.
// Nodes without a type field default to object in JSON Schema, but
// we only apply the strict-mode object rules to nodes that opt in
// explicitly — otherwise we'd rewrite description-only wrappers.
func isObjectType(node map[string]any) bool {
	t, ok := node["type"]
	if !ok {
		// If there are properties but no explicit type, treat it as
		// an object — that's the only sensible interpretation and it
		// matches what strict mode will infer.
		_, hasProps := node["properties"]
		return hasProps
	}
	s, ok := t.(string)
	return ok && s == "object"
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
