#!/usr/bin/env bash
# .claude/hooks/cortex-session-start.sh
#
# SessionStart hook. Prints a one-line status summary for the cortex
# stack and, if an observation trail is already exported via
# CORTEX_TRAIL_ID, surfaces that fact so Claude knows follow-up
# observations will attach to an existing bundle.
#
# stdout is injected as additional context on SessionStart events.
# Failures are silent by design: the hook must never block a session.
set -u

# Resolve cortex from PATH first, then the project-local build.
if command -v cortex >/dev/null 2>&1; then
  CORTEX=cortex
elif [[ -n "${CLAUDE_PROJECT_DIR:-}" && -x "${CLAUDE_PROJECT_DIR}/cortex" ]]; then
  CORTEX="${CLAUDE_PROJECT_DIR}/cortex"
else
  exit 0
fi

# cortex status is the same shallow probe `cortex doctor` uses; it stays
# well under the SessionStart budget.
status=$("$CORTEX" status 2>/dev/null)
rc=$?

if (( rc != 0 )); then
  # Stack is down. Hint the user/agent at the remediation without failing
  # the session — cortex is optional for many tasks.
  echo "Cortex: managed stack is not reachable. Run 'cortex up' to enable observe/recall."
  exit 0
fi

# Trim `cortex status` down to the healthy-summary lines the agent cares
# about. The first line is the header; the rest are per-component.
printf 'Cortex is available. %s\n' "$(printf '%s' "$status" | awk 'NR>1 {printf "%s; ", $0}' | sed 's/; $//')"

if [[ -n "${CORTEX_TRAIL_ID:-}" ]]; then
  printf 'Active cortex trail in environment: %s — cortex observe will auto-attach to this trail until you run cortex trail end.\n' "$CORTEX_TRAIL_ID"
fi

# Ingest freshness check. Compare the last ingested commit for the
# `cortex` project to the current git HEAD. If they differ, hint the
# agent to re-ingest before doing architecture work so module summaries
# reflect the current source tree.
#
# This is a hint, not an auto-trigger: ingest is LLM-backed and can take
# minutes, so we never run it implicitly. The agent decides whether the
# current task warrants the cost.
if command -v git >/dev/null 2>&1; then
  project_dir="${CLAUDE_PROJECT_DIR:-$PWD}"
  current_head=$(git -C "$project_dir" rev-parse --short HEAD 2>/dev/null || true)
  if [[ -n "$current_head" ]]; then
    ingest_status=$("$CORTEX" ingest status --project=cortex 2>/dev/null || true)
    if [[ -n "$ingest_status" ]]; then
      last_commit=$(printf '%s' "$ingest_status" | sed -n 's/.*last_commit=\([^ ]*\).*/\1/p')
      if [[ -z "$last_commit" ]]; then
        printf 'Cortex: project "cortex" has never been ingested. For architecture, refactoring, or cross-module work, run:\n  ./cortex ingest --project=cortex --commit=%s .\n' "$current_head"
      elif [[ "$last_commit" != "$current_head"* && "$current_head" != "$last_commit"* ]]; then
        printf 'Cortex: ingest is stale (last=%s, HEAD=%s). For architecture or refactoring work, re-ingest with:\n  ./cortex ingest --project=cortex --commit=%s .\n' "$last_commit" "$current_head" "$current_head"
      fi
    else
      printf 'Cortex: project "cortex" has never been ingested. For architecture, refactoring, or cross-module work, run:\n  ./cortex ingest --project=cortex --commit=%s .\n' "$current_head"
    fi
  fi
fi
