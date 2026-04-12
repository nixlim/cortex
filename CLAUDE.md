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

This project is indexed by GitNexus as **cortex** (3952 symbols, 11174 relationships, 300 execution flows). Use the GitNexus **CLI** (`gitnexus` binary on PATH) to understand code, assess impact, and navigate safely.

## 🚨 CRITICAL: gitnexus is a CLI, NOT an MCP tool in this session

**All gitnexus commands in this document MUST be invoked via the shell using the `gitnexus` binary, not as MCP tool calls.** Use `Bash` with the `gitnexus` CLI instead.

Every example in this document shows the CLI form: `gitnexus impact <target> --direction=upstream --repo=cortex`. When you see older documentation referring to `gitnexus_foo({bar: baz})` style calls, **translate them to the CLI** — they are equivalent, and the CLI is what actually works here.

### Multi-repo disambiguation (required)
The `gitnexus` CLI indexes multiple projects on this machine. Every command MUST include `--repo=cortex` or gitnexus will return a "Multiple repositories indexed" error. If you forget, the error message lists the available repos.

### Canonical CLI form

| Operation | CLI command |
|---|---|
| Blast radius | `gitnexus impact <symbol> --direction=upstream --repo=cortex` |
| 360° context | `gitnexus context <symbol> --repo=cortex` |
| Search by concept | `gitnexus query "<phrase>" --repo=cortex` |
| Raw Cypher | `gitnexus cypher "<cypher>" --repo=cortex` |
| Index status | `gitnexus status` |
| Refresh index | `npx gitnexus analyze` (in repo root) |
| List indexed repos | `gitnexus list` |

> If any `gitnexus` command warns the index is stale or reports zero upstream callers for a symbol you know has callers, run `npx gitnexus analyze` in the terminal before retrying.

## Always Do

- **MUST run `gitnexus impact <symbol> --direction=upstream --repo=cortex` before editing any function, class, or method.** Report the blast radius (direct callers, affected processes, risk level) to the user before touching the symbol.
- **MUST warn the user** if impact analysis returns HIGH or CRITICAL risk before proceeding.
- When exploring unfamiliar code, use `gitnexus query "<concept>" --repo=cortex` to find execution flows instead of grepping. Results are grouped by process and ranked by relevance.
- When you need the 360° view of a specific symbol, use `gitnexus context <symbol> --repo=cortex`.

## When Debugging

1. `gitnexus query "<error or symptom>" --repo=cortex` — find execution flows related to the issue.
2. `gitnexus context <suspect function> --repo=cortex` — see all callers, callees, and process participation.
3. For regressions: inspect `git diff` against the main branch and re-run impact on anything you touched.

## When Refactoring

- **Extracting/Splitting**: run `gitnexus context <target> --repo=cortex` to see all incoming/outgoing refs, then `gitnexus impact <target> --direction=upstream --repo=cortex` to find all external callers before moving code.
- **Renaming**: gitnexus-CLI renames are not supported in this session; use careful grep + manual review instead. Do **not** trust find-and-replace without reading every call site — the CLI does not expose a safe rename command.
- After any refactor: rebuild the index with `npx gitnexus analyze` and re-run impact on the moved/renamed symbols to confirm the call graph still looks right.

## Never Do

- **NEVER** call `gitnexus_impact(...)` or any `gitnexus_*` function as if it were an MCP tool. It is NOT registered in this session. Use `Bash` with the `gitnexus` CLI binary.
- NEVER edit a function, class, or method without first running `gitnexus impact` on it via the CLI.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis.
- NEVER rename symbols with find-and-replace — read every call site first, because the CLI does not cover this.

## Self-Check Before Finishing

Before completing any code modification task, verify:
1. `gitnexus impact` was run for every modified symbol.
2. No HIGH/CRITICAL risk warnings were ignored.
3. Changes match the expected scope (verify with `git status` + manual review; `gitnexus detect_changes` is an MCP-only feature, unavailable here).
4. All direct (depth=1) dependents were updated.

## Impact Risk Levels

| Depth | Meaning | Action |
|-------|---------|--------|
| d=1 | WILL BREAK — direct callers/importers | MUST update these |
| d=2 | LIKELY AFFECTED — indirect deps | Should test |
| d=3 | MAY NEED TESTING — transitive | Test if critical path |

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
