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
