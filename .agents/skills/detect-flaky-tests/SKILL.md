---
name: detect-flaky-tests
description: >
  Detects flaky tests by analyzing the last 7 days of Prow presubmit runs for
  kubernetes-sigs/agent-sandbox — the blocking test-unit and test-e2e jobs, using the
  structured junit XML artifacts each run archives to GCS. For each newly-detected
  flaky test or recurring infra failure, opens a GitHub issue with full evidence and a
  draft fix PR. Does not touch BigQuery, dashboards, or any external storage — those
  are cron-job concerns layered on top.
---

# Detect Flaky Tests

A test is **flaky** when it produces both PASS and FAIL outcomes across multiple
independent CI runs in the last 7 days, with no code change explaining the
inconsistency. Cross-PR analysis is the strongest signal: the PR number is part of
every run's GCS path, so a test failing on PR-A while passing on PR-B is almost
certainly non-determinism, not a regression.

## Flakiness threshold (keeps false-positive rate low)

A test is flagged only when **all three** hold in the 7-day window:

| Condition | Rationale |
|---|---|
| `fail_count >= 2` | One failure could be noise |
| `pass_count >= 2` | One pass could be a lucky run |
| `0.05 < fail_rate < 0.95` | Outside this band it is reliably broken or reliably passing |

## Jobs and lanes

Only the **blocking** presubmits produce flake signal (optional jobs like the kops
benchmarks and kwok scalability are manually triggered, env-heavy, and excluded):

| Prow job | junit artifacts | Lane name |
|---|---|---|
| `presubmit-agent-sandbox-test-unit` | `junit_unit-go.xml`, `junit_unit-go-<module>.xml`, `junit_unit-python-<pkg>.xml` | filename minus `junit_`/`.xml` (e.g. `unit-go`, `unit-python-k8s-agent-sandbox`) |
| `presubmit-agent-sandbox-test-e2e` | `junit_e2e-go.xml`, `junit_e2e-python-sdk.xml`, `junit_e2e-typescript-sdk.xml` | same rule (e.g. `e2e-go`) |

Track results per `(test_name, lane)`. A test flaky in one lane is flagged even if
clean in others.

---

## Step 1 — Enumerate runs (last 7 days)

Prow archives every run under
`gs://kubernetes-ci-logs/pr-logs/pull/kubernetes-sigs_agent-sandbox/<PR>/<job>/<build>/`
and maintains a flat index whose file mtimes match run completion:

```bash
for JOB in presubmit-agent-sandbox-test-unit presubmit-agent-sandbox-test-e2e; do
  gcloud storage ls -l "gs://kubernetes-ci-logs/pr-logs/directory/$JOB/" \
    | awk -v cutoff="$(date -u -v-7d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ)" \
      '$2 >= cutoff && $3 ~ /[0-9]+\.txt$/ {print $3}'
done
```

Each `<build>.txt` pointer contains the run's full GCS path (which embeds the PR
number). Fetch pointers in bulk, not one `gcloud storage cat` per file — write the
list to a file and use `gcloud storage cat -I` or xargs with batching; a week is a
few hundred runs per job.

For each run path, fetch:
- `finished.json` — `result` (SUCCESS/FAILURE/ABORTED) and completion timestamp
- `artifacts/junit_*.xml` — the structured test results

Cache per-run parsed results under `/tmp/agent-sandbox-flaky/<build>.json` and reuse
across invocations, but aggregate ONLY over runs inside the current 7-day window.
macOS purges /tmp every ~3 days — if the cache is missing, rebuild it, and verify
window coverage (parsed + skipped accounts for every enumerated run) before
aggregating.

## Step 2 — Parse junit

For every `junit_*.xml` in a run:

```python
import xml.etree.ElementTree as ET
root = ET.fromstring(data)
for case in root.iter("testcase"):
    name, classname = case.get("name"), case.get("classname", "")
    if case.find("skipped") is not None: continue
    failed = case.find("failure") is not None or case.find("error") is not None
    # record (name, classname, lane, failed) — lane = filename minus junit_/.xml
```

`classname` holds the Go package or Python module — record it as `package`.
Subtests appear as `TestParent/SubTest`; keep the full name.

## Step 3 — Infra triage (BEFORE aggregating)

A run is an **infra failure** (contributes zero test results) when
`finished.json` says FAILURE but the expected junit files are absent or empty.
For those runs, fetch `build-log.txt` (same GCS dir) and classify:

| Signal | Log pattern |
|---|---|
| kind cluster creation | `failed to create cluster`, `node(s) not ready`, `timed out waiting` |
| Image build/pull | `failed to pull image`, `ErrImagePull`, buildx/registry errors |
| Module/pip fetch | `proxy.golang.org` errors, `pip install` network failures |
| Runner resources | `OOMKilled`, `No space left on device` |
| Boskos/project lease | `failed to acquire project`, boskos errors |
| Setup exit | a `dev/ci/presubmits/*` step fails before any test output |

If junit files exist but the job still failed, the failure is test-level — use the
junit verdicts. If junit is truncated mid-suite (far fewer testcases than sibling
runs), count only the recorded results and note the truncation. An infra pattern
appearing in ≥2 runs warrants an issue (Step 5b); infra runs never count toward any
test's fail_count.

## Step 4 — Aggregate and verify

Build the per-`(test_name, lane)` table over non-infra runs:
`fail_count, pass_count, total_runs, fail_rate`, then apply the threshold.

**Cross-PR verification (mandatory before flagging):** the PR number is in each
run's path. If all of a candidate's failures come from a single PR — or the test
only exists on that PR's branch (new test under development) — it is a PR
regression, NOT flake. Exclude it and note the exclusion in the report.

## Step 5a — Issues for flaky tests

Dedup first (an issue "covers" a test if its title matches `flaky: <TEST_NAME>` or
its body names the test):

```bash
gh issue list -R kubernetes-sigs/agent-sandbox --state open --search "flaky: <TEST_NAME>" --json number,title
gh issue list -R kubernetes-sigs/agent-sandbox --state open --search "<TEST_NAME>" --json number,title
```

If none, create with the repo's real labels (`kind/flake` exists — use it):

```bash
gh issue create -R kubernetes-sigs/agent-sandbox \
  --title "flaky: <TEST_NAME>" --label "kind/flake" \
  --body "<evidence: lane, package, runs analysed, fail/pass counts, flake rate,
          infra runs excluded, links to 2-3 failing and 1-2 passing Prow runs
          (https://prow.k8s.io/view/gs/kubernetes-ci-logs/<run path>),
          note that a draft fix PR follows>"
```

## Step 5b — Issues for recurring infra failures

Same dedup pattern with title `infra: <PATTERN_SUMMARY>`; label `kind/failing-test`
(no infra label exists — do not create labels). Include occurrence count, example
Prow links, and a 3-5 line log excerpt. No fix PR for infra issues.

## Step 6 — Draft fix PR per new flaky test

Diagnose from the test source before writing anything. Repo-specific patterns on
top of the usual Go/Python ones:

| Pattern | Fix |
|---|---|
| Timing threshold too tight at warm-pool speeds | Widen with an absolute floor (see #1553 — a 0.2s warm-adopt baseline makes fraction-of-serialized limits sit inside API jitter) |
| `kubectl wait` label-selector race before objects exist | Wait on the owning resource's status instead (e.g. `sandboxwarmpool` `.status.readyReplicas`) |
| Sandbox/pod readiness assumed | `framework` predicates / `require.Eventually` on conditions, not sleeps |
| Cleanup race between tests | Wait for deletion in `t.Cleanup`; claims/sandboxes have finalizer latencies |
| Python e2e event-loop/asyncio timing | Compare against measurable baselines; avoid sub-second absolute limits |
| Shared cluster state (pools, namespaces) | Unique names per test; never assume pool spares |

Branch `fix/flaky-<test-name-kebab>` from main; commit
`fix(tests): resolve flakiness in <TestName>` with `Fixes #<issue>`; open a
**draft** PR with root cause, fix, evidence (rates + Prow links), and a test plan
(`go test -race -count=10` for unit; 3+ suite reruns for e2e).

## Step 7 — Report

Two tables — flaky tests (Test | Lane | Package | Flake rate | Fail/Pass | Infra
excluded | Issue | Fix PR | Action) and infra issues (Pattern | Job | Occurrences |
Issue | Action) — plus a short list of excluded false-positive candidates with
reasons. If nothing found: `No new flaky tests or infra issues detected.`

## Notes

- This skill performs no storage/dashboard writes; the invoking cron layers
  BigQuery snapshots on top.
- GCS reads need no auth for this public bucket, but `gcloud storage` on this
  project is the tested path.
- Runs older than the bucket's retention or with expired artifacts: skip and note.
- Never create labels; if a label is missing, omit it and mention that in the issue.
