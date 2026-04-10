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
    # Count edits to any file that could plausibly carry intent,
    # invariants, or design decisions worth observing. This covers:
    #   - source code (*.go, *.sh, *.py, *.rs, *.ts, *.js, *.proto, ...)
    #   - docs and specs (*.md, *.mdx, *.rst, *.txt under docs/)
    #   - configuration (*.yaml, *.yml, *.toml, *.json, *.ini, Dockerfile)
    #   - agent-facing surfaces (CLAUDE.md, AGENTS.md, .claude/**)
    #
    # Docs and config are included deliberately (user request): writing
    # a design doc forces articulation of intent, and writing a config
    # often surfaces undocumented invariants — both are high-signal
    # observation triggers.
    #
    # Explicit noise exclusions: lockfiles, caches, generated output,
    # ephemeral stores. These are never worth observing even in bulk.
    target=$(printf '%s' "$payload" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')
    [[ -z "$target" ]] && exit 0

    # Exclude first — a path under a noise directory is never counted
    # regardless of its extension.
    case "$target" in
      */node_modules/*|*/vendor/*|*/dist/*|*/build/*|*/.cache/*|*/.gitnexus/*|*/.beads/*|*/.cortex/*|*/.venv/*|*/__pycache__/*|*/.next/*|*/target/*)
        exit 0 ;;
      *.lock|*go.sum|*package-lock.json|*yarn.lock|*Cargo.lock|*.log|*.tmp|*.bak|*.swp|*.DS_Store)
        exit 0 ;;
    esac

    should_count=0
    case "$target" in
      # Source code
      *.go|*.sh|*.bash|*.zsh|*.py|*.rs|*.ts|*.tsx|*.js|*.jsx|*.mjs|*.java|*.kt|*.swift|*.c|*.cc|*.cpp|*.h|*.hpp|*.rb|*.proto|*.sql)
        should_count=1 ;;
      # Docs and specs
      *.md|*.mdx|*.rst|*.adoc|*/docs/*.txt|*/docs/*)
        should_count=1 ;;
      # Config / ops surfaces
      *.yaml|*.yml|*.toml|*.json|*.ini|*.cfg|*Dockerfile*|*Makefile*|*.mk|*.tf|*.hcl)
        should_count=1 ;;
      # Agent surfaces — always track so the agent's own playbook
      # changes get observed.
      *CLAUDE.md|*AGENTS.md|*/.claude/*)
        should_count=1 ;;
    esac

    if (( should_count == 1 )); then
      touch "$pending"
      count=$(cat "$edit_counter" 2>/dev/null || echo 0)
      echo $((count + 1)) > "$edit_counter"
    fi
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
