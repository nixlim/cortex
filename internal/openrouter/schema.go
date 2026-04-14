package openrouter

import (
	"encoding/json"
	"fmt"
	"sort"
)

// normalizeSchema rewrites a JSON schema to satisfy strict-mode
// json_schema constraints as enforced by OpenAI-compatible providers
// (OpenRouter forwards the response_format to the upstream model
// verbatim, so the upstream's strict rules apply):
//
//  1. Every object schema MUST set additionalProperties:false.
//  2. Every object schema's "required" array MUST list every key in
//     "properties" — strict mode does not accept optional fields.
//  3. Nullable types are rejected. Both `"type": "null"` and
//     `"type": ["string", "null"]` fail with an error pointing at the
//     offending path.
//
// This normalizer is a lifted copy of internal/openai.normalizeSchema
// to keep the two provider packages decoupled. See the package doc
// for the rationale.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("openrouter: empty schema")
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("openrouter: decode schema: %w", err)
	}
	rootMap, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openrouter: root schema must be a JSON object")
	}
	if t, _ := rootMap["type"].(string); t != "" && t != "object" {
		return nil, fmt.Errorf("openrouter: root schema must be type=object, got %q", t)
	}
	if _, present := rootMap["type"]; !present {
		rootMap["type"] = "object"
	}
	if err := walk(rootMap, "$"); err != nil {
		return nil, err
	}
	out, err := json.Marshal(rootMap)
	if err != nil {
		return nil, fmt.Errorf("openrouter: re-marshal schema: %w", err)
	}
	return out, nil
}

func walk(node map[string]any, path string) error {
	if err := checkTypeForNull(node, path); err != nil {
		return err
	}

	if isObjectType(node) {
		if _, ok := node["additionalProperties"]; !ok {
			node["additionalProperties"] = false
		}
		if props, ok := node["properties"].(map[string]any); ok {
			keys := make([]string, 0, len(props))
			for k := range props {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			node["required"] = toAnySlice(keys)

			for k, v := range props {
				if sub, ok := v.(map[string]any); ok {
					if err := walk(sub, path+".properties."+k); err != nil {
						return err
					}
				}
			}
		}
	}

	if items, ok := node["items"].(map[string]any); ok {
		if err := walk(items, path+".items"); err != nil {
			return err
		}
	}

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

func checkTypeForNull(node map[string]any, path string) error {
	t, present := node["type"]
	if !present {
		return nil
	}
	switch v := t.(type) {
	case string:
		if v == "null" {
			return fmt.Errorf("openrouter: strict mode does not support null types at %s", path)
		}
	case []any:
		for _, elem := range v {
			if s, ok := elem.(string); ok && s == "null" {
				return fmt.Errorf("openrouter: strict mode does not support null types at %s", path)
			}
		}
	}
	return nil
}

func isObjectType(node map[string]any) bool {
	t, ok := node["type"]
	if !ok {
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
