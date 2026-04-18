package summarise

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// buildCommunityPrompt constructs the prompt argv for one per-
// community Claude call. The central instruction is curate rather
// than summarise, because the brief is read by future agents who
// will NOT re-read the raw observations — so the brief must answer
// "what does an agent need to know to act?" not "what happened
// here?". Design record: bead cortex-8sr notes + cortex-z33 slot
// revision.
//
// The prompt embeds the community's observations inline as a JSON
// block rather than a looser prose dump so the LLM can address
// entries by id when promoting an exemplar, nominating conflicts, or
// citing open questions.
func buildCommunityPrompt(project string, community Community, membershipHash string) string {
	var sb strings.Builder

	sb.WriteString("You are curating a cluster of ")
	sb.WriteString(itoa(len(community.Entries)))
	sb.WriteString(" episodic observations from the `")
	sb.WriteString(project)
	sb.WriteString("` project. The cluster was auto-grouped by Leiden community detection, so the observations share an underlying concern but vary in quality, age, and usefulness.\n\n")

	sb.WriteString("Your job is NOT to summarise. It is to make this cluster useful to a future agent who will read ONLY your brief — not the raw observations. A useful brief:\n")
	sb.WriteString(" 1. Names the through-line in a 3-8 word theme_label noun phrase.\n")
	sb.WriteString(" 2. States the single canonical_insight in one sentence a new engineer could act on.\n")
	sb.WriteString(" 3. Promotes exactly one exemplar_entry_id — the single observation worth re-reading if any.\n")
	sb.WriteString("    Optionally list up to three alternate_exemplars for equally load-bearing alternatives.\n")
	sb.WriteString(" 4. Lists open_questions that remain unresolved across the cluster.\n")
	sb.WriteString(" 5. Flags suggested_conflicts: pairs/triples of entry_ids where observations disagree, each with a short `nature` explanation. Be conservative — only flag genuine disagreement, not topical variation. The agent reading this will validate; do not assert contradictions you are not confident in.\n")
	sb.WriteString(" 6. Identifies stable concept_tags (short noun phrases) that let future recall cross-link to other clusters.\n")
	sb.WriteString(" 7. Writes a summary paragraph (< 1200 chars) threading the canonical insight through the supporting observations. Prefer signal over completeness; do not restate every entry.\n")
	sb.WriteString(" 8. Notes in coverage_notes what the brief deliberately OMITS (stale workarounds, superseded decisions, off-topic noise).\n\n")

	sb.WriteString("You MUST set community_id=")
	sb.WriteString(string(community.ID))
	sb.WriteString(" and membership_hash=")
	sb.WriteString(membershipHash)
	sb.WriteString(" verbatim in your output so the caller can match your brief to the cluster it was derived from.\n\n")

	sb.WriteString("Return JSON matching the provided schema. No prose outside the JSON.\n\n")

	sb.WriteString("<observations>\n")
	// Serialise observations as JSON so the model can reliably
	// reference by id. A compact array keeps tokens low.
	type promptEntry struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		TS   string `json:"ts"`
		Body string `json:"body"`
	}
	rows := make([]promptEntry, 0, len(community.Entries))
	for _, e := range community.Entries {
		rows = append(rows, promptEntry{
			ID:   e.ID,
			Kind: e.Kind,
			TS:   e.TS.UTC().Format(time.RFC3339),
			Body: e.Body,
		})
	}
	// json.Marshal is used for escaping rather than prettiness; the
	// LLM does not need indentation and it would cost tokens.
	buf, _ := json.Marshal(rows)
	sb.Write(buf)
	sb.WriteString("\n</observations>\n")

	return sb.String()
}

// buildProjectPrompt constructs the stitch prompt. It takes the
// entire current set of CommunityBrief slot-maps (both freshly
// summarised and carried over from prior runs) and asks for a single
// ProjectBrief that an agent reads when orienting on the project.
//
// The stitch is a "useful map," not a table of contents: it should
// surface top themes, concepts that span multiple clusters, and
// project-level open questions aggregating per-cluster opens. The
// input per brief is a compact JSON projection (not the full brief)
// to keep the stitch call's context bounded.
func buildProjectPrompt(project string, briefs []map[string]any, generatedAt time.Time) string {
	var sb strings.Builder

	sb.WriteString("You are producing one top-level ProjectBrief for the `")
	sb.WriteString(project)
	sb.WriteString("` project. Your input is ")
	sb.WriteString(itoa(len(briefs)))
	sb.WriteString(" CommunityBriefs. An agent orienting on this project will read ONLY your output — not the per-community briefs.\n\n")

	sb.WriteString("A useful ProjectBrief is an ACTIONABLE MAP, not a table of contents. It:\n")
	sb.WriteString(" 1. Lists top_themes ranked by importance to a working agent, each linking back to its community_id and naming the canonical_insight that theme turns on.\n")
	sb.WriteString(" 2. Identifies cross_cluster_concepts: concepts that appear in multiple communities (same noun phrase in >=2 communities' concept_tags). Each entry names the concept and the community_ids it spans.\n")
	sb.WriteString(" 3. Aggregates open_questions_project — unresolved threads from per-community open_questions that are still live at the project level. Deduplicate and rephrase; do not just concatenate.\n")
	sb.WriteString(" 4. Writes a stitched_narrative paragraph (< 2400 chars) threading the top themes into one readable map. NOT a summary of summaries — a narrative that helps an agent decide where to look first.\n")
	sb.WriteString(" 5. Sets coverage_ratio to an honest estimate of the fraction of the project's observations these briefs cover (1.0 = all clusters summarised, lower if some communities failed or were skipped this run).\n\n")

	sb.WriteString("You MUST set project=")
	sb.WriteString(project)
	sb.WriteString(" and generated_at=")
	sb.WriteString(generatedAt.UTC().Format(time.RFC3339))
	sb.WriteString(" verbatim, and community_ids MUST list every community_id present in your input in the order provided.\n\n")

	sb.WriteString("Return JSON matching the provided schema. No prose outside the JSON.\n\n")

	sb.WriteString("<community_briefs>\n")
	buf, _ := json.Marshal(briefs)
	sb.Write(buf)
	sb.WriteString("\n</community_briefs>\n")

	return sb.String()
}

// itoa avoids the strconv import for a single small integer.
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
