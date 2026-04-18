package summarise

// CommunityBriefSchema is the JSON Schema passed to Claude Code CLI
// via --json-schema on each per-community call. It mirrors the slot
// contract declared in internal/frames/builtin/community_brief.json:
// required = {community_id, membership_hash, theme_label,
// canonical_insight, exemplar_entry_id, summary}, optional =
// {alternate_exemplars, open_questions, suggested_conflicts,
// concept_tags, coverage_notes}.
//
// The schema is deliberately tight: additionalProperties:false
// because the model occasionally hallucinates extra fields, and per-
// field type + length bounds keep the output compact enough for the
// stitch pass to consume N briefs without blowing the context
// window.
const CommunityBriefSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "community_id",
    "membership_hash",
    "theme_label",
    "canonical_insight",
    "exemplar_entry_id",
    "summary"
  ],
  "properties": {
    "community_id":       {"type": "string", "minLength": 1, "maxLength": 128},
    "membership_hash":    {"type": "string", "pattern": "^[0-9a-f]{64}$"},
    "theme_label":        {"type": "string", "minLength": 3, "maxLength": 80},
    "canonical_insight":  {"type": "string", "minLength": 10, "maxLength": 400},
    "exemplar_entry_id":  {"type": "string", "minLength": 1, "maxLength": 128},
    "summary":            {"type": "string", "minLength": 20, "maxLength": 1200},
    "alternate_exemplars": {
      "type": "array",
      "maxItems": 3,
      "items": {"type": "string", "minLength": 1, "maxLength": 128}
    },
    "open_questions": {
      "type": "array",
      "maxItems": 8,
      "items": {"type": "string", "minLength": 5, "maxLength": 240}
    },
    "suggested_conflicts": {
      "type": "array",
      "maxItems": 8,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["entry_ids", "nature"],
        "properties": {
          "entry_ids": {
            "type": "array",
            "minItems": 2,
            "maxItems": 4,
            "items": {"type": "string", "minLength": 1, "maxLength": 128}
          },
          "nature": {"type": "string", "minLength": 5, "maxLength": 240}
        }
      }
    },
    "concept_tags": {
      "type": "array",
      "maxItems": 12,
      "items": {"type": "string", "minLength": 2, "maxLength": 60}
    },
    "coverage_notes": {"type": "string", "maxLength": 400}
  }
}`

// ProjectBriefSchema mirrors internal/frames/builtin/project_brief.json:
// required = {project, generated_at, community_ids,
// stitched_narrative}, optional = {top_themes, cross_cluster_concepts,
// open_questions_project, coverage_ratio}.
const ProjectBriefSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["project", "generated_at", "community_ids", "stitched_narrative"],
  "properties": {
    "project":             {"type": "string", "minLength": 1, "maxLength": 128},
    "generated_at":        {"type": "string", "minLength": 20, "maxLength": 40},
    "community_ids": {
      "type": "array",
      "items": {"type": "string", "minLength": 1, "maxLength": 128}
    },
    "stitched_narrative":  {"type": "string", "minLength": 20, "maxLength": 2400},
    "top_themes": {
      "type": "array",
      "maxItems": 12,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["theme_label", "community_id"],
        "properties": {
          "theme_label":       {"type": "string", "minLength": 3, "maxLength": 80},
          "community_id":      {"type": "string", "minLength": 1, "maxLength": 128},
          "canonical_insight": {"type": "string", "minLength": 10, "maxLength": 400}
        }
      }
    },
    "cross_cluster_concepts": {
      "type": "array",
      "maxItems": 24,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["concept", "community_ids"],
        "properties": {
          "concept":       {"type": "string", "minLength": 2, "maxLength": 60},
          "community_ids": {
            "type": "array",
            "minItems": 2,
            "items": {"type": "string", "minLength": 1, "maxLength": 128}
          }
        }
      }
    },
    "open_questions_project": {
      "type": "array",
      "maxItems": 12,
      "items": {"type": "string", "minLength": 5, "maxLength": 240}
    },
    "coverage_ratio": {"type": "number", "minimum": 0, "maximum": 1}
  }
}`
