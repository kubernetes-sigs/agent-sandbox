---
name: triage-issues
description: Triage open GitHub issues for kubernetes-sigs/agent-sandbox by mapping them to roadmap.md and assigning k8s priority labels + Kanban Priority (P0–P4) on Project #120. Use when asked to triage issues, prioritize the backlog, or sync issue priorities to the project board.
---

# Triage agent-sandbox issues against the roadmap

Map every open issue in `kubernetes-sigs/agent-sandbox` to a roadmap theme/item, assign
a triage priority, and sync that priority to both the issue's k8s `priority/*` label and
the **Priority** field (P0–P4) on the Kanban board.

## Label ↔ Priority mapping (fixed)

| k8s label | Kanban Priority |
|---|---|
| `priority/critical-urgent` | P0 |
| `priority/important-soon` | P1 |
| `priority/important-longterm` | P2 |
| `priority/backlog` | P3 |
| `priority/awaiting-more-evidence` | P4 |

## Key references

- Roadmap: `roadmap.md` in the repo root (source of truth for themes + status).
- Project board: https://github.com/orgs/kubernetes-sigs/projects/120 (owner `kubernetes-sigs`, number `120`).
- The board's `Priority` single-select field holds P0–P4.

## Triage heuristics

Assign each open issue a tier using, in order:

1. **Honor existing `priority/*` labels** — do not downgrade/overwrite an existing priority label unless the issue clearly changed; note any you keep.
2. **Roadmap references** — if the issue number is cited in `roadmap.md`, inherit that item's status: `⏳ In Progress` → **P1**, `📅 Planned` → **P2**, `✅ Completed` follow-ups → P2/P3.
3. **Bug severity** — data-integrity / duplicate-resource / resource-leak bugs in the core controller → **P0/P1**. Self-labeled "Critical" → P0.
4. **Value props** — latency / performance / scale bottlenecks skew **P1** (called out as a primary value proposition).
5. **Security** — critical security vulnerabilities / CVEs → **P0** (`priority/critical-urgent`); routine dependency/patch updates → **P1** (tied to "Security Fixes" roadmap item).
6. **Low-signal** — empty bodies, one-line questions, "collecting use cases", pinned/frozen community threads, items marked `triage/not-reproducible`/`triage/needs-information` → **P4**. Nice-to-have features, refactors, cleanups, test scaffolding → **P3**.

## Procedure

1. **Verify access**: `gh auth status`; confirm repo `kubernetes-sigs/agent-sandbox`.
2. **Read the roadmap**: read `roadmap.md`; extract themes, item statuses, and any cited issue numbers.
3. **Fetch open issues**:
   `gh issue list --repo kubernetes-sigs/agent-sandbox --state open --limit 500 --json number,title,labels,assignees,createdAt,updatedAt,body,url > open_issues.json`
4. **Discover board metadata** (IDs change per board, always re-fetch):
   - Project + Priority field/options: `gh project field-list 120 --owner kubernetes-sigs --format json` → grab the `Priority` field id and the P0–P4 option ids; project id via `gh project view 120 --owner kubernetes-sigs --format json`.
   - Item ids: `gh project item-list 120 --owner kubernetes-sigs --limit 1000 --query "is:open" --format json` → map issue number → project item id. Add any open issue not yet on the board with `gh project item-add 120 --owner kubernetes-sigs --url <issue-url> --format json`.
5. **Triage**: assign every open issue exactly one tier using the heuristics. Sanity-check that the assigned set equals the open-issue set (no missing, no extras).
6. **Report first**: write a markdown report (`issue-triage-report.md`) grouping issues by priority with roadmap mapping + one-line rationale, and a "second look" section for judgment calls (e.g., heuristic conflicts, boundary cases, insufficient info, or ambiguous roadmap items). **In an interactive run, stop and wait for the user to review before changing anything on GitHub.** In an automated/scheduled run, see below.
7. **Apply** (after approval in interactive mode, or filtered by untriaged issues in scheduled mode):
   - Label: `gh issue edit <#> --repo kubernetes-sigs/agent-sandbox --add-label "<priority/...>" --remove-label "<priority/...>"` (if the tier changed).
   - Board field: `gh project item-edit --id <itemId> --project-id <projectId> --field-id <fieldId> --single-select-option-id <optionId>`.
   - Drive the apply loop from a small script so all issues are processed in one pass (adding a `sleep 1` delay between API calls to respect GitHub's 900 points/min secondary rate limit); collect per-issue ok/err and print a final `total / errors` summary.
8. **Confirm**: report the final distribution (count per tier) and any errors.

## Scheduled / automated mode

When invoked non-interactively (e.g. via cron), there is no human to approve the report.
In that mode:
- Still write `issue-triage-report.md` as an audit artifact.
- **Only apply changes to issues that currently have NO `priority/*` label** (new/untriaged issues), so a daily run is incremental and never silently overrides a human's manual prioritization.
- For issues that already have a priority label, leave them as-is but list them in the report under "already triaged".
- Post the run summary (counts applied, counts skipped, errors) as the final message.

## Notes

- **Roadmap priority changes** are reconciled only via interactive runs, not scheduled mode. When `roadmap.md` changes the priority of a theme, an interactive run re-triages every open issue against the current roadmap and surfaces the resulting diffs for a human to approve. Scheduled mode deliberately leaves already-triaged issues untouched (see above), so it never silently re-prioritizes existing issues based on a roadmap edit. To propagate a roadmap change to issues that already have a `priority/*` label, run the skill interactively and apply the proposed changes.
- All `priority/*` labels and the P0–P4 options already exist on the repo/board; no need to create them.
- `gh project` GraphQL IDs (project, field, options, items) are not stable across boards — always re-discover them at runtime; never hardcode.
