#!/usr/bin/env bash
# .claude/hooks/cortex-observation-nudge.sh
#
# Stop hook. Runs when Claude finishes its response turn. Checks the
# per-session state left by cortex-track-edits.sh and, if the turn
# produced code edits but no cortex observation was written, blocks the
# stop with a reason that forces Claude to persist the findings before
# the conversation returns to the user.
#
# This is the enforcement layer behind the cortex-guide skill's
# "MUST observe every valuable finding" rule. The skill says what to
# do; this hook catches the cases where the agent forgot.
#
# The nudge only fires once per pending-flag cycle — after blocking,
# the hook removes the pending marker so the next Stop event doesn't
# loop infinitely. Claude observes, the next turn's Stop sees a clean
# state, and the conversation proceeds.
set -u

command -v jq >/dev/null 2>&1 || exit 0

payload=$(cat)

# If the Stop hook was triggered by Claude Code's own retry loop
# (stop_hook_active=true) we must NOT block again. Re-blocking causes
# infinite loops.
active=$(printf '%s' "$payload" | jq -r '.stop_hook_active // false')
[[ "$active" == "true" ]] && exit 0

session_id=$(printf '%s' "$payload" | jq -r '.session_id // empty')
[[ -z "$session_id" ]] && exit 0

state_dir="${TMPDIR:-/tmp}/cortex-claude-${session_id}"
pending="${state_dir}/pending-observation"
edit_counter="${state_dir}/edit-count"

# No pending edits → nothing to nudge.
[[ ! -f "$pending" ]] && exit 0

count=$(cat "$edit_counter" 2>/dev/null || echo 0)
# A single trivial edit is noisy to enforce on. Only block when enough
# real work has accumulated that a fact is likely.
(( count < 2 )) && exit 0

# Clear the marker BEFORE emitting the block so the next Stop event
# after Claude observes isn't blocked a second time.
rm -f "$pending" "$edit_counter"

reason="You made ${count} code or docs edit(s) this turn but wrote no cortex observation. Per .claude/skills/cortex/cortex-guide, you MUST record root causes, decisions, surprising behaviour, benchmark results, docs intent, and config invariants to cortex before stopping so future sessions can retrieve them. Write one or more observations now with: ./cortex observe \"<one-sentence claim>\" --kind=<Observation|SessionReflection|ObservedRace> --facets=domain:<area>,project:cortex,subsystem:<name>. Only those three kinds are valid for direct writes — the semantic frames (design_decision, architecture_note, bug_pattern, etc.) are populated by cortex reflect, not by observe. To tag a record as a decision use --kind=Observation with facet kind-of-record:decision. If the edits were pure busywork (renames, formatting, dependency bumps, typo fixes) with no persistent fact worth recording, say so in one line and stop."

jq -n --arg reason "$reason" '{
  decision: "block",
  reason: $reason
}'
