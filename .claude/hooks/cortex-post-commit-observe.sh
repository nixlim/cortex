#!/usr/bin/env bash
# .claude/hooks/cortex-post-commit-observe.sh
#
# PostToolUse hook wired to Bash(git commit *). Fires after every
# successful git commit the agent runs and writes one cortex observation
# capturing the commit's subject, body, and SHA so the commit shows up
# in future `cortex recall` queries without the agent having to
# remember to observe it.
#
# Silent no-op on any failure path (cortex down, commit failed, jq
# missing, parse error) — this hook must never break a commit.
#
# Facet convention:
#   kind          = Decision
#   domain        = Repo
#   project       = cortex
#   commit        = <sha>
#   branch        = <current branch>
#
# Duplicate-write protection: the hook records each processed commit
# SHA in a per-session marker file and skips re-observation on replays.
set -u

if command -v cortex >/dev/null 2>&1; then
  CORTEX=cortex
elif [[ -n "${CLAUDE_PROJECT_DIR:-}" && -x "${CLAUDE_PROJECT_DIR}/cortex" ]]; then
  CORTEX="${CLAUDE_PROJECT_DIR}/cortex"
else
  exit 0
fi

command -v jq  >/dev/null 2>&1 || exit 0
command -v git >/dev/null 2>&1 || exit 0

payload=$(cat)

# Confirm this was a real git commit invocation. The `if` filter in
# settings.json already narrows to Bash(git commit *), but double-check
# the tool_input shape in case the filter evolves.
tool=$(printf '%s' "$payload" | jq -r '.tool_name // empty')
[[ "$tool" == "Bash" ]] || exit 0

cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // empty')
[[ "$cmd" == *"git commit"* ]] || exit 0

# Skip --amend rewrites — amends produce a new SHA for the same semantic
# change and we'd double-observe. The git log observation of the prior
# commit already captured the work.
[[ "$cmd" == *"--amend"* ]] && exit 0

# Confirm the tool actually succeeded. A failed commit (pre-commit hook
# rejected, nothing staged, etc.) must not produce an observation.
is_error=$(printf '%s' "$payload" | jq -r '.tool_response.isError // .tool_response.is_error // false')
[[ "$is_error" == "true" ]] && exit 0

cwd=$(printf '%s' "$payload" | jq -r '.cwd // empty')
project_dir="${cwd:-${CLAUDE_PROJECT_DIR:-$PWD}}"
cd "$project_dir" 2>/dev/null || exit 0

# Pull the committed work directly from git instead of trying to
# reverse-engineer it from the tool_input — git is authoritative.
sha=$(git rev-parse --short HEAD 2>/dev/null) || exit 0
[[ -z "$sha" ]] && exit 0

# Dedupe: if we already observed this SHA in this session, skip.
session_id=$(printf '%s' "$payload" | jq -r '.session_id // "unknown"')
marker_dir="${TMPDIR:-/tmp}/cortex-claude-${session_id}"
mkdir -p "$marker_dir" 2>/dev/null || exit 0
marker="${marker_dir}/observed-commits"
touch "$marker"
if grep -qxF "$sha" "$marker"; then
  exit 0
fi
echo "$sha" >> "$marker"

subject=$(git log -1 --format='%s' 2>/dev/null)
body=$(git log -1 --format='%b' 2>/dev/null)
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
files=$(git diff-tree --no-commit-id --name-only -r HEAD 2>/dev/null | head -8 | tr '\n' ',' | sed 's/,$//')

# Collapse the observation body into a single line so it reads cleanly
# in recall output. Keep the subject intact; append the first paragraph
# of the body if one exists, then the top file list.
observation="commit ${sha}: ${subject}"
if [[ -n "$body" ]]; then
  first_para=$(printf '%s' "$body" | awk 'BEGIN{RS=""} NR==1 {print; exit}' | tr '\n' ' ' | sed 's/  */ /g' | sed 's/^ *//; s/ *$//')
  if [[ -n "$first_para" ]]; then
    observation+=" — ${first_para}"
  fi
fi
if [[ -n "$files" ]]; then
  observation+=" (files: ${files})"
fi

# Cap observation length — very long commit bodies bloat recall output.
max=600
if (( ${#observation} > max )); then
  observation="${observation:0:$max}…"
fi

facets="domain:Repo,project:cortex,commit:${sha}"
[[ -n "$branch" ]] && facets+=",branch:${branch}"

"$CORTEX" observe "$observation" --kind=Decision --facets="$facets" >/dev/null 2>&1 || true
exit 0
