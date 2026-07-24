---
name: test-pyramid
description: Analyze the repo's unit and E2E tests and propose rebalancing toward a test pyramid — which E2E tests (or assertions inside them) can be covered by unit tests, which unit-level gaps genuinely need E2E coverage, and where coverage is duplicated. Use when the user asks about test pyramid, test rebalancing, "should this be an e2e or unit test", E2E-to-unit migration, or slow/flaky E2E suites that might shrink.
---

# Test Pyramid Analysis

Goal: **anything that can be tested as a unit test should be a unit test; only what genuinely needs a real cluster should be E2E.** The end state is a pyramid — many fast unit tests, few E2E tests. This skill produces an evidence-backed rebalancing report; it does NOT move tests itself unless the user asks afterward.

## Phase 1 — Inventory

Enumerate both layers and count actual test functions (not just files), so the report can show the pyramid shape numerically:

- **Unit tests:** `git ls-files '*_test.go' | grep -v '^test/e2e/'` plus `git ls-files 'test/e2e/framework/*_test.go'` and Python unit tests under `clients/python/**/test/unit/` (discover with `git ls-files 'clients/python/*' | grep '/test/unit/'` — there is more than one such directory). Count `func Test...` per package (`grep -c '^func Test'`) and `def test_` for Python.
- **E2E / system tests:** `test/e2e/**/*_test.go` excluding `test/e2e/framework/` (kind-cluster E2E, incl. `test/e2e/extensions/`), `test/e2e/clients/python/` (SDK E2E), `dev/tools/test-migration.py` (upgrade/rollback), `test/stress/` (load). Count `func Test...` and, for table-driven E2E, the sub-scenarios.
- Note per-layer runtime cost if discoverable (CI job durations from `dev/ci/`, TestGrid tab names) — the payoff argument for each migration is time and flake surface removed from presubmit.

## Phase 2 — Characterize every E2E test

Read each E2E test body (fan out parallel subagents over batches of 3-5 files for speed; each returns structured notes). For every test, record:

1. **What it arranges** (objects applied, cluster preconditions).
2. **What it asserts** — split assertions into:
   - **Cluster-physics assertions:** pod actually scheduled/running, kubelet behavior, image pulls, real networking/routing (sandbox-router paths), LoadBalancer/Gateway, RBAC enforcement, webhook admission via real API server, CRD conversion via real storage, controller<->controller timing, upgrade/rollback state survival.
   - **Logic assertions:** field values on objects after a reconcile, label/annotation stamping, status conditions, owner references, name hashing, defaulting, spec conversion, error classification, requeue decisions — anything a reconciler computes deterministically from inputs.
3. **The seam:** which function/reconciler produces each logic assertion's value (e.g. `isAdoptable`, `computeAndSetStatus`, `merge_flaky_by_test`). If you cannot name the seam, you cannot claim a unit test can cover it.

## Phase 3 — Classify

- **E2E → unit candidate:** every logic assertion whose seam is reachable with the repo's existing unit patterns — table-driven tests with `controller-runtime`'s fake client and `newScheme(t)` (see `extensions/controllers/sandboxclaim_controller_test.go` for the house style), direct function calls, or Python `unittest.mock`. A whole E2E test is a removal candidate only if ALL its assertions are logic assertions; otherwise propose extracting the logic assertions to unit tests and thinning the E2E to its cluster-physics core.
- **Keep as E2E:** tests dominated by cluster-physics assertions. Do not propose unit-testing scheduling, pod readiness, real router traffic, webhook round-trips, or migration/upgrade behavior — a fake client proves nothing there.
- **Unit → E2E promotion (rare, keep the pyramid in mind):** unit tests that mock so much they only test the mock (assert the seam is meaningfully exercised), or critical user journeys (create claim → adopt → route traffic → shutdown) with no E2E smoke path at all. Prefer ONE thin journey test over per-feature E2E.
- **Redundant coverage:** the same seam asserted at both layers with the same inputs — keep the unit test, list the E2E assertion as prunable.

## Phase 4 — Verify before reporting

Every suggestion must survive these checks (drop or downgrade to "uncertain" if not):

- Name the exact seam (file:line of the function) and confirm it is callable without a cluster — no unexported entanglement with live clients that fake clients can't satisfy.
- Check a unit test for that seam doesn't already exist (use package-qualified symbol search across `*_test.go` and verify the test invokes the function); if it exists, the finding is "redundant E2E assertion", not "missing unit test".
- Confirm the E2E test would still have a reason to exist after extraction, or explicitly state it can be deleted and what residual smoke coverage (if any) replaces it.
- Beware behaviors that LOOK like logic but are cluster-coupled: informer cache timing, conversion-webhook storage effects, owner-reference garbage collection, Prow retest semantics, anything the migration test covers. When unsure, classify as keep-E2E and say why.

## Phase 5 — Report (inline only; write no files)

1. **Pyramid snapshot:** test-function counts per layer (unit / E2E / migration / stress) now vs. after adopting all suggestions, plus CI-time estimate if available.
2. **E2E → unit table:** E2E test (file:line) | assertions to extract | seam (file:line) | proposed unit test location + shape (table-driven case to add vs. new test) | what remains of the E2E test (thinned / deleted).
3. **Unit → E2E table** (expect this to be short — the pyramid demands it): gap | why unit level cannot cover it | proposed E2E home (existing file to extend before new file).
4. **Redundant coverage list.**
5. **Top 5 quick wins** ranked by (CI time + flake history removed) vs. effort — cross-reference flake evidence where available: open `kind/flake` issues, TestGrid history for the presubmit tabs, or `dev/tools/flake-report` output if that tool exists in your checkout. Migrating a flaky E2E assertion to a unit test is the highest-value move.

Rank suggestions by confidence; separate "verified" (seam confirmed, patterns exist) from "needs maintainer judgment" (cluster-coupling unclear). Do not inflate the E2E→unit list — a wrong migration that deletes real coverage is worse than a kept E2E test.
