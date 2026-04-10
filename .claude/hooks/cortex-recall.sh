#!/usr/bin/env bash
# .claude/hooks/cortex-recall.sh
#
# UserPromptSubmit hook. Reads the JSON hook payload from stdin, extracts
# the user's prompt, runs `cortex recall` against it, and emits an
# additionalContext string so Claude sees prior-session observations that
# match what the user just asked.
#
# This is a soft nudge, not a wall: if cortex is down, the stack is cold,
# or recall returns nothing, the hook exits 0 with no context injected and
# the session proceeds normally.
#
# Input schema (from Claude Code):
#   { "hook_event_name": "UserPromptSubmit", "prompt": "<user text>", ... }
#
# Output schema:
#   { "hookSpecificOutput": { "hookEventName": "UserPromptSubmit",
#                             "additionalContext": "..." } }
#
# Requires: cortex on PATH, jq. If either is missing, the hook is a no-op.

set -u

# Resolve the cortex binary: prefer PATH, fall back to the project-local
# build so contributors who haven't `install`-ed cortex still get the
# hook. CLAUDE_PROJECT_DIR is set by Claude Code for every hook invocation.
if command -v cortex >/dev/null 2>&1; then
  CORTEX=cortex
elif [[ -n "${CLAUDE_PROJECT_DIR:-}" && -x "${CLAUDE_PROJECT_DIR}/cortex" ]]; then
  CORTEX="${CLAUDE_PROJECT_DIR}/cortex"
else
  exit 0
fi

# jq is required for both payload parsing and output assembly.
command -v jq >/dev/null 2>&1 || exit 0

payload=$(cat)

prompt=$(printf '%s' "$payload" | jq -r '.prompt // empty' 2>/dev/null)
[[ -z "$prompt" ]] && exit 0

# Skip recall for very short prompts (keywords like "yes", "ok", "continue"
# are unlikely to produce useful hits and just waste context budget).
len=${#prompt}
(( len < 12 )) && exit 0

# Skip pure slash-command invocations — the user is explicitly driving a
# skill/command, not asking a knowledge question.
[[ "$prompt" =~ ^/[a-zA-Z] ]] && exit 0

# Bound recall so a slow stack can't delay the prompt. 3 results is enough
# to prime the agent without dominating context.
recall_json=$("$CORTEX" recall "$prompt" --limit=3 --json 2>/dev/null)
rc=$?
(( rc != 0 )) && exit 0
[[ -z "$recall_json" ]] && exit 0

# Extract results and drop out early if empty.
count=$(printf '%s' "$recall_json" | jq '.Results | length' 2>/dev/null)
[[ -z "$count" || "$count" == "null" || "$count" == "0" ]] && exit 0

# Build a compact, reader-friendly summary of the top hits.
summary=$(printf '%s' "$recall_json" | jq -r '
  "Cortex recall surfaced " + (.Results | length | tostring) + " prior observation(s) relevant to this prompt. Consult these before answering; read the full entry bodies if any look decisive.\n\n" +
  (
    .Results
    | to_entries
    | map(
        "  [\(.key + 1)] \(.value.EntryID)  score=\(.value.Score | tostring | .[0:5])\n      \(.value.Body)"
        + (if .value.TrailContext != "" then "\n      trail: \(.value.TrailContext)" else "" end)
        + (if .value.CommunityContext != "" then "\n      community: \(.value.CommunityContext)" else "" end)
      )
    | join("\n\n")
  )
' 2>/dev/null)
[[ -z "$summary" ]] && exit 0

# Emit the additionalContext envelope Claude Code expects.
jq -n --arg ctx "$summary" '{
  hookSpecificOutput: {
    hookEventName: "UserPromptSubmit",
    additionalContext: $ctx
  }
}'
