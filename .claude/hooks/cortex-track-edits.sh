#!/usr/bin/env bash
# .claude/hooks/cortex-track-edits.sh
#
# PostToolUse hook wired to Write|Edit|MultiEdit. Records the fact that
# code edits happened in this session into a per-session state directory
# so the Stop hook (cortex-observation-nudge.sh) can detect
# edits-without-observations and remind the agent to write an
# observation before stopping.
#
# Also recognises `cortex observe` and `cortex trail end` Bash
# invocations and clears the "pending" flag, so the Stop hook only
# fires when there are genuinely unrecorded edits.
#
# Silent on every failure path — must never break a tool call.
set -u

command -v jq >/dev/null 2>&1 || exit 0

payload=$(cat)

session_id=$(printf '%s' "$payload" | jq -r '.session_id // empty')
[[ -z "$session_id" ]] && exit 0

state_dir="${TMPDIR:-/tmp}/cortex-claude-${session_id}"
mkdir -p "$state_dir" 2>/dev/null || exit 0

pending="${state_dir}/pending-observation"
edit_counter="${state_dir}/edit-count"

tool=$(printf '%s' "$payload" | jq -r '.tool_name // empty')

case "$tool" in
  Write|Edit|MultiEdit|NotebookEdit)
    # Skip edits to paths that are trivially not code. Docs updates,
    # CLAUDE.md churn, and log/cache writes don't need an observation
    # nudge. The check is heuristic — anything under cmd/, internal/,
    # pkg/, .claude/hooks/ counts as code; everything else is ignored.
    target=$(printf '%s' "$payload" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')
    [[ -z "$target" ]] && exit 0
    case "$target" in
      */cmd/*|*/internal/*|*/pkg/*|*/.claude/hooks/*|*.go|*.sh)
        touch "$pending"
        count=$(cat "$edit_counter" 2>/dev/null || echo 0)
        echo $((count + 1)) > "$edit_counter"
        ;;
    esac
    ;;
  Bash)
    cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // empty')
    # An observe or trail end call clears the pending flag.
    if [[ "$cmd" == *"cortex observe"* || "$cmd" == *"cortex trail end"* ]]; then
      rm -f "$pending" "$edit_counter"
    fi
    ;;
esac

exit 0
