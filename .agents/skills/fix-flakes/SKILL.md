---
name: fix-flakes
description: Diagnose and fix flaky tests tracked as open kind/flake issues in kubernetes-sigs/agent-sandbox — reproduce the flake, apply a minimal fix, and open a PR linking the issue. Use when asked to fix a flake, work through kind/flake issues, or when run on a schedule after dev/tools/flake-report has filed flake issues.
---

# Fix flaky tests from kind/flake issues

Work through open `kind/flake` issues (filed nightly by `dev/tools/flake-report`),
reproduce each flake, land a minimal fix, and open a PR that closes the issue.
Evidence before edits: never change a test you could not observe failing or
whose failure output you have not read.

## Inputs

- A specific issue number, if the caller gives one; otherwise discover work:
  `gh issue list --repo kubernetes-sigs/agent-sandbox --label kind/flake --state open --json number,title,body`
- Skip any issue that already has an open fix PR: search
  `gh pr list --repo kubernetes-sigs/agent-sandbox --state open --search "<issue number> in:body"`
  and check the results actually reference `Fixes #<n>` / `Closes #<n>`.
- Skip infra-failure issues (title contains "infrastructure failures") unless
  explicitly asked — those are CI tooling work (`dev/ci/`, prow job config in
  `kubernetes/test-infra`), not test edits. If you do take one, the fix lives in
  `dev/ci/shared/runner.py` retries, image prepulls, or job resources.

## Reproduce first

1. Read the issue: test name, tab/job, failure timestamps, job-history link.
   Fetch a failing run's junit + build log from the GCS links to read the real
   error before touching anything.
2. Locate the test: `git grep -n "func <TestName>("` (Go) or
   `git grep -n "def <test_name>("` (Python SDK e2e). For a Go subtest
   (`TestFoo/subtest`), grep for the parent (`func TestFoo(`) only — the part
   before the first `/` — then find the `t.Run("subtest", ...)` inside it.
3. Stress it:
   - Unit tests: `go test ./<package>/... -run '^<TestName>$' -race -count=50`
     (for a subtest, `-run '^TestFoo$/^subtest$'`)
   - e2e tests need a cluster: `make deploy-kind` first (see AGENTS.md), then
     `go test ./test/e2e/... -run '^<TestName>$' -count=10` with
     `KUBECONFIG=bin/KUBECONFIG`.
4. If it will not reproduce locally after a reasonable stress run, do NOT
   guess-fix. Analyze the CI failure output instead; if the cause is still
   unclear, post your findings as a comment on the issue and stop.

## Diagnose — the usual suspects

- Missing eventual-consistency handling: asserting on controller-driven state
  without polling. Fix by waiting with a deadline (see existing helpers in
  `test/e2e/`), not by sleeping.
- Timeouts tuned for fast machines: CI under Docker-in-Docker is slow. If a
  timeout must grow, justify it in a comment; prefer replacing fixed sleeps
  with polling.
- Shared state between parallel tests: cluster-scoped resources, fixed names,
  fixed ports. Namespace or randomize per test.
- Ordering assumptions on lists/maps, time-of-day assumptions, leaked
  goroutines from a previous test (`-race` output helps).

## Fix and verify

- Minimal diff, repo conventions (AGENTS.md). Never disable or skip a test to
  make it green; quarantine decisions belong to maintainers on the issue.
- Re-run the stress loop from "Reproduce" — it must pass repeatedly
  (unit: `-count=50` clean; e2e: `-count=10` clean).
- Run `make test-unit` (and the affected e2e suite if applicable).

## Deliver

- Branch `flake-fix/issue-<n>`, one issue per branch/PR.
- Commit style matches recent history (e.g. `fix(e2e): wait for sandbox ready
  in TestFoo (#<n>)`).
- PR body: `Fixes #<n>`, the diagnosis in two or three sentences, and the
  before/after stress-run evidence (command + result).
- If the fix is a revert-worthy product bug rather than a test bug, say so on
  the issue and file/label accordingly instead of papering over it in tests.

## Scheduled / automated mode

When invoked non-interactively (e.g. via `dev/tools/flake-fix` on cron):

- Process at most the number of issues the caller specifies (default 1) so a
  run stays bounded.
- Only act on issues where reproduction or CI failure output confirms the
  diagnosis; otherwise leave an analysis comment on the issue and move on.
- Push the branch and open the PR autonomously, then report: issues examined,
  PRs opened, issues commented, issues skipped and why.
