# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **cortex** (4237 symbols, 10864 relationships, 300 execution flows). Use GitNexus to understand code, assess impact, and navigate safely.

## ⚠️ CRITICAL: Use the CLI, not MCP tools

**ALWAYS invoke GitNexus via its CLI (`npx gitnexus <subcommand>` from the Bash tool), NOT via the `mcp__gitnexus__*` tools.** The CLI is the canonical interface for this project. Do not use `mcp__gitnexus__query`, `mcp__gitnexus__context`, `mcp__gitnexus__impact`, `mcp__gitnexus__cypher`, `mcp__gitnexus__detect_changes`, `mcp__gitnexus__rename`, `mcp__gitnexus__api_impact`, `mcp__gitnexus__route_map`, `mcp__gitnexus__tool_map`, `mcp__gitnexus__shape_check`, `mcp__gitnexus__list_repos`, or any other `mcp__gitnexus__*` tool. Run the CLI instead.

- Every command below is a `Bash` tool invocation. All other GitNexus guidance in this document refers to CLI subcommands, not MCP calls.
- The repository is indexed as `cortex`. Pass `-r cortex` when you want to be explicit, or rely on the current working directory.
- If `npx gitnexus status` reports the index is stale, run `npx gitnexus analyze` before any other GitNexus command.
- Two operations have no CLI equivalent: pre-commit change detection (use `git diff --name-only --cached` / `git status --short`) and symbol renames (use `npx gitnexus context <name>` + `npx gitnexus impact <name>` first, then apply edits via the Edit tool). Do NOT fall back to the MCP tools for these.

## Always Do

- **MUST run impact analysis before editing any symbol.** Before modifying a function, class, or method, run `npx gitnexus impact <symbolName> --direction upstream` and report the blast radius (direct callers, affected processes, risk level) to the user.
- **MUST verify staged scope before committing** via `git status --short` and `git diff --stat --cached`. If you want graph-level verification, re-run `npx gitnexus impact` on the changed symbols and confirm the d=1 set is what you intended.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding with edits.
- When exploring unfamiliar code, use `npx gitnexus query "concept"` to find execution flows instead of grepping. It returns process-grouped results ranked by relevance.
- When you need full context on a specific symbol — callers, callees, which execution flows it participates in — use `npx gitnexus context <symbolName>`.

## When Debugging

1. `npx gitnexus query "<error or symptom>"` — find execution flows related to the issue
2. `npx gitnexus context <suspect function>` — see all callers, callees, and process participation
3. `npx gitnexus cypher "MATCH (p:Process {name: '<processName>'})-[:CONTAINS]->(s) RETURN s"` — trace the full execution flow step by step
4. For regressions: `git diff main...HEAD` — see what your branch changed, then re-run `npx gitnexus impact` on any modified symbol to understand downstream risk

## When Refactoring

- **Renaming**: MUST run `npx gitnexus context <old>` and `npx gitnexus impact <old>` first to see every caller. Apply the rename via the Edit tool with `replace_all: true` on each affected file, then re-run `npx gitnexus analyze` to rebuild the graph and re-run `npx gitnexus impact <new>` to confirm no callers were missed. Do NOT use the `mcp__gitnexus__rename` MCP tool.
- **Extracting/Splitting**: MUST run `npx gitnexus context <target>` to see all incoming/outgoing refs, then `npx gitnexus impact <target> --direction upstream` to find all external callers before moving code.
- After any refactor: `git diff --stat` to verify only expected files changed; re-run `npx gitnexus analyze` and `npx gitnexus impact` on the new symbol names to confirm the call graph is intact.

## Never Do

- **NEVER use the `mcp__gitnexus__*` tools.** Always invoke `npx gitnexus ...` via Bash.
- NEVER edit a function, class, or method without first running `npx gitnexus impact` on it.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with blind find-and-replace — run `npx gitnexus context` + `impact` first to enumerate callers.
- NEVER commit changes without inspecting `git status --short` + `git diff --stat --cached` to verify scope.

## CLI Quick Reference

All commands are invoked via the `Bash` tool. Add `-r cortex` if ambiguity arises.

| Task | CLI invocation |
|------|----------------|
| Find code by concept | `npx gitnexus query "auth validation"` |
| 360-degree view of one symbol | `npx gitnexus context validateUser` |
| Blast radius before editing | `npx gitnexus impact validateUser --direction upstream` |
| Downstream dependency walk | `npx gitnexus impact validateUser --direction downstream` |
| Custom graph queries | `npx gitnexus cypher "MATCH (f:Function)-[:CALLS]->(g) WHERE f.name = 'foo' RETURN g.name"` |
| Index status | `npx gitnexus status` |
| List indexed repos | `npx gitnexus list` |
| Re-index (after commits) | `npx gitnexus analyze` (add `--embeddings` to preserve embeddings) |
| Generate wiki | `npx gitnexus wiki` |
| Pre-commit scope check | `git status --short && git diff --stat --cached` (no GitNexus CLI equivalent) |
| Pre-rename caller enumeration | `npx gitnexus context <name> && npx gitnexus impact <name>` (no GitNexus CLI rename equivalent) |

## Impact Risk Levels

| Depth | Meaning | Action |
|-------|---------|--------|
| d=1 | WILL BREAK — direct callers/importers | MUST update these |
| d=2 | LIKELY AFFECTED — indirect deps | Should test |
| d=3 | MAY NEED TESTING — transitive | Test if critical path |

## Overview Queries (no MCP resources)

The MCP `gitnexus://repo/...` resources are NOT to be used. Get the same information from the CLI:

| Need | CLI invocation |
|------|----------------|
| Codebase overview + freshness | `npx gitnexus status` |
| All functional areas / clusters | `npx gitnexus cypher "MATCH (c:Cluster) RETURN c.name, c.size ORDER BY c.size DESC"` |
| All execution flows | `npx gitnexus cypher "MATCH (p:Process) RETURN p.name ORDER BY p.name"` |
| Step-by-step execution trace for one process | `npx gitnexus cypher "MATCH (p:Process {name: '<name>'})-[:CONTAINS]->(s) RETURN s.name, s.file, s.start_line ORDER BY s.start_line"` |

## Self-Check Before Finishing

Before completing any code modification task, verify:
1. `npx gitnexus impact` was run for all modified symbols (via the Bash tool, not MCP)
2. No HIGH/CRITICAL risk warnings were ignored
3. `git status --short` + `git diff --stat --cached` confirm the staged scope matches what you intended
4. All d=1 (WILL BREAK) dependents were updated

## Keeping the Index Fresh

After committing code changes, the GitNexus index becomes stale. Re-run analyze to update it:

```bash
npx gitnexus analyze
```

If the index previously included embeddings, preserve them by adding `--embeddings`:

```bash
npx gitnexus analyze --embeddings
```

To check whether embeddings exist, inspect `.gitnexus/meta.json` — the `stats.embeddings` field shows the count (0 means no embeddings). **Running analyze without `--embeddings` will delete any previously generated embeddings.**

> Claude Code users: A PostToolUse hook handles this automatically after `git commit` and `git merge`.

## CLI

| Task | Read this skill file |
|------|---------------------|
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
