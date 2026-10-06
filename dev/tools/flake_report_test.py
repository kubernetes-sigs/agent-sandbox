# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Unit tests for flake-report — red-run classification and infra counting."""

import importlib.util
import json
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

# Load the extensionless flake-report script via importlib (same pattern as
# dev/tools/latest_published_tag_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_SCRIPT_PATH = os.path.join(_SCRIPT_DIR, "flake-report")
_loader = SourceFileLoader("flake_report", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("flake_report", _loader)
flake_report = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(flake_report)


def artifacts(**files):
    """fetch_artifact stand-in serving canned finished.json etc. per name."""
    data = {name.replace("_", "-") + ".json": value for name, value in files.items()}
    return data.get


class ClassifyRedRunTest(unittest.TestCase):
    def test_aborted_run_is_not_a_failure(self):
        cls = flake_report.classify_red_run(
            False, False, artifacts(finished={"result": "ABORTED"}))
        self.assertEqual(cls, "aborted")

    def test_aborted_wins_even_with_junit_errors(self):
        # A run cancelled mid-flight can leave junit "errors" behind; the
        # tests it killed must not be tallied as flakes.
        cls = flake_report.classify_red_run(
            True, True, artifacts(finished={"result": "ABORTED"}))
        self.assertEqual(cls, "aborted")

    def test_lowercase_aborted_from_crier_is_not_infra(self):
        # crier writes a lowercase "aborted" result when the pod died before
        # podutils uploaded finished.json; those runs must not count as infra.
        cls = flake_report.classify_red_run(
            False, False, artifacts(finished={"result": "aborted"}))
        self.assertEqual(cls, "aborted")

    def test_failing_testcase_is_a_test_failure(self):
        cls = flake_report.classify_red_run(
            True, True,
            artifacts(finished={"result": "FAILURE", "revision": "abc"}))
        self.assertEqual(cls, "test_failure")

    def test_junit_with_no_failing_testcase_is_pretest_breakage(self):
        # mypy/vet/compile gates fail after junit was written: the job goes
        # red with a clean junit — the PR's own breakage, not infra.
        cls = flake_report.classify_red_run(
            False, True,
            artifacts(finished={"result": "FAILURE", "revision": "abc"}))
        self.assertEqual(cls, "pretest_failure")

    def test_clone_merge_conflict_is_a_clone_failure(self):
        cls = flake_report.classify_red_run(
            False, False,
            artifacts(
                finished={"result": "FAILURE"},
                clone_records=[
                    {"refs": {"repo": ""}},
                    {"refs": {"repo": "agent-sandbox"}, "failed": True,
                     "commands": [{"command": "git merge --no-ff abc",
                                   "output": "CONFLICT (content): Merge "
                                             "conflict in examples/README.md\n"
                                             "Automatic merge failed",
                                   "error": "exit status 1"}]},
                ],
            ))
        self.assertEqual(cls, "clone_failure")

    def test_clone_failed_without_conflict_is_infra(self):
        # clonerefs also sets failed=true on network/ref-fetch errors; only
        # a verified merge conflict may be blamed on a stale PR.
        cls = flake_report.classify_red_run(
            False, False,
            artifacts(
                finished={"result": "FAILURE"},
                clone_records=[
                    {"refs": {"repo": "agent-sandbox"}, "failed": True,
                     "commands": [{"command": "git fetch origin",
                                   "output": "",
                                   "error": "connection timed out"}]},
                ],
            ))
        self.assertEqual(cls, "infra")

    def test_missing_clone_records_with_no_revision_stays_infra(self):
        # Without clone-records.json the cause cannot be proven benign.
        cls = flake_report.classify_red_run(
            False, False, artifacts(finished={"result": "FAILURE"}))
        self.assertEqual(cls, "infra")

    def test_no_junit_clean_clone_is_infra(self):
        cls = flake_report.classify_red_run(
            False, False,
            artifacts(
                finished={"result": "FAILURE", "revision": "abc"},
                clone_records=[{"refs": {"repo": "agent-sandbox"}}],
            ))
        self.assertEqual(cls, "infra")

    def test_unfetchable_artifacts_stay_infra(self):
        # When GCS gives us nothing we cannot prove a benign cause; keep the
        # pre-existing conservative behavior.
        cls = flake_report.classify_red_run(False, False, lambda name: None)
        self.assertEqual(cls, "infra")

    def test_clone_records_not_fetched_when_junit_present(self):
        fetched = []

        def fetch(name):
            fetched.append(name)
            return {"result": "FAILURE", "revision": "abc"} \
                if name == "finished.json" else None

        flake_report.classify_red_run(False, True, fetch)
        self.assertEqual(fetched, ["finished.json"])


class ColumnBuildIdTest(unittest.TestCase):
    def test_takes_last_segment_when_id_carries_a_name_prefix(self):
        # Live dashboards emit '\ue000<build-id>', but be robust to a
        # '<name>\ue000<build-id>' layout too: the build ID is always last.
        self.assertEqual(
            flake_report.column_build_id(["job\ue000123"], 0), "123")

    def test_strips_testgrid_id_prefix(self):
        self.assertEqual(
            flake_report.column_build_id(["\ue0002098490053683580928"], 0),
            "2098490053683580928")

    def test_missing_or_empty_columns(self):
        self.assertIsNone(flake_report.column_build_id([], 0))
        self.assertIsNone(flake_report.column_build_id([""], 0))
        self.assertIsNone(flake_report.column_build_id(["\ue000"], 0))


class MakeArtifactFetcherTest(unittest.TestCase):
    def test_resolves_pr_logs_directory_pointer(self):
        urls = []

        def fake_fetch_text(url):
            urls.append(url)
            return "gs://bucket/pr-logs/pull/org_repo/1/job/123\n"

        def fake_fetch_json(url):
            urls.append(url)
            return {"result": "FAILURE"}

        with mock.patch.object(flake_report, "fetch_text", fake_fetch_text), \
             mock.patch.object(flake_report, "fetch_json", fake_fetch_json):
            fetch = flake_report.make_artifact_fetcher(
                "bucket/pr-logs/directory/job", "123")
            self.assertEqual(fetch("finished.json"), {"result": "FAILURE"})
            # The pointer is cached: a second artifact costs one fetch.
            fetch("clone-records.json")
        self.assertEqual(urls, [
            "https://storage.googleapis.com/bucket/pr-logs/directory/job/123.txt",
            "https://storage.googleapis.com/bucket/pr-logs/pull/org_repo/1/job/123/finished.json",
            "https://storage.googleapis.com/bucket/pr-logs/pull/org_repo/1/job/123/clone-records.json",
        ])

    def test_periodic_logs_path_needs_no_pointer(self):
        urls = []

        def fake_fetch_json(url):
            urls.append(url)
            return {"result": "ABORTED"}

        with mock.patch.object(flake_report, "fetch_json", fake_fetch_json):
            fetch = flake_report.make_artifact_fetcher("bucket/logs/job", "456")
            self.assertEqual(fetch("finished.json"), {"result": "ABORTED"})
        self.assertEqual(
            urls, ["https://storage.googleapis.com/bucket/logs/job/456/finished.json"])

    def test_returns_none_without_build_id_or_on_fetch_error(self):
        fetch = flake_report.make_artifact_fetcher("bucket/logs/job", None)
        self.assertIsNone(fetch("finished.json"))

        def boom(url):
            raise RuntimeError("404")

        with mock.patch.object(flake_report, "fetch_json", boom):
            fetch = flake_report.make_artifact_fetcher("bucket/logs/job", "456")
            self.assertIsNone(fetch("finished.json"))


def rle(values):
    return [{"value": v, "count": 1} for v in values]


class AnalyzeTabTest(unittest.TestCase):
    """analyze_tab with a canned TestGrid table and canned GCS artifacts.

    Six columns, newest first:
      0: aborted run that also left junit errors behind
      1: stale-PR clone failure (no junit)
      2: pre-test PR breakage (junit present, nothing failed, job red)
      3: true infra failure (no junit, clone fine)
      4: real test failure of TestA
      5: green run (TestA passed on a bare retest of column 4's PR)
    """

    TABLE = {
        "query": "kubernetes-ci-logs/pr-logs/directory/job",
        "changelists": ["b0", "b1", "b2", "b3", "b4", "b4"],
        "column_ids": ["\ue000b0", "\ue000b1", "\ue000b2",
                       "\ue000b3", "\ue000b4", "\ue000b5"],
        "timestamps": [600, 500, 400, 300, 200, 100],
        "tests": [
            {"name": "job.Overall", "statuses": rle([12, 12, 12, 12, 12, 1])},
            {"name": "job.Pod", "statuses": rle([12, 12, 12, 12, 12, 1])},
            {"name": "pkg.TestA", "statuses": rle([12, 0, 1, 0, 12, 1])},
            {"name": "pkg.TestB", "statuses": rle([0, 0, 1, 0, 1, 1])},
        ],
    }

    ARTIFACTS = {
        "b0": {"finished.json": {"result": "ABORTED"}},
        "b1": {"finished.json": {"result": "FAILURE"},
               "clone-records.json": [
                   {"failed": True,
                    "commands": [{"output": "Automatic merge failed"}]}]},
        "b2": {"finished.json": {"result": "FAILURE", "revision": "abc"}},
        "b3": {"finished.json": {"result": "FAILURE", "revision": "abc"},
               "clone-records.json": [{"failed": False}]},
        "b4": {"finished.json": {"result": "FAILURE", "revision": "abc"}},
    }

    def analyze(self):
        def fake_fetcher(gcs_query, build_id):
            return lambda name: self.ARTIFACTS.get(build_id, {}).get(name)

        with mock.patch.object(flake_report, "fetch_json",
                               return_value=self.TABLE), \
             mock.patch.object(flake_report, "make_artifact_fetcher",
                               fake_fetcher):
            return flake_report.analyze_tab("dash", "tab", 6)[:3]

    def test_red_runs_split_into_causes(self):
        _, _, infra = self.analyze()
        self.assertEqual(infra["red_runs"], 5)
        self.assertEqual(infra["infra_runs"], 1)
        self.assertEqual(infra["aborted_runs"], 1)
        self.assertEqual(infra["clone_failures"], 1)
        self.assertEqual(infra["pretest_failures"], 1)
        self.assertEqual(infra["total_runs"], 6)
        # The only infra column is 3.
        self.assertEqual(infra["last_failure_ts"], 300)
        # Existing consumers of --json rely on these keys.
        self.assertLessEqual(
            {"tab", "red_runs", "infra_runs", "total_runs",
             "last_failure_ts", "job_history"},
            set(infra))

    def test_aborted_junit_errors_do_not_count_as_flakes(self):
        flaky, consistent, _ = self.analyze()
        self.assertEqual(consistent, [])
        (finding,) = flaky
        self.assertEqual(finding["test"], "pkg.TestA")
        # Column 0 failed inside an aborted run and must not be tallied:
        # only column 4 counts, and the newest real failure is at ts=200.
        self.assertEqual(finding["fails"], 1)
        self.assertEqual(finding["last_failure_ts"], 200)
        self.assertEqual(finding["retest_flips"], 1)

    def test_aborted_pass_does_not_create_a_retest_flip(self):
        # TestP fails on changelist c1 and "passes" only inside the aborted
        # rerun of the same changelist; that pass must not count as a flip.
        table = {
            "query": "kubernetes-ci-logs/pr-logs/directory/job",
            "changelists": ["c1", "c1", "c2"],
            "column_ids": ["\ue000a0", "\ue000a1", "\ue000a2"],
            "timestamps": [300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 12, 1])},
                {"name": "pkg.TestP", "statuses": rle([1, 12, 1])},
                {"name": "pkg.TestQ", "statuses": rle([13, 1, 1])},
            ],
        }
        art = {"a0": {"finished.json": {"result": "ABORTED"}},
               "a1": {"finished.json": {"result": "FAILURE",
                                        "revision": "abc"}}}

        def fake_fetcher(gcs_query, build_id):
            return lambda name: art.get(build_id, {}).get(name)

        with mock.patch.object(flake_report, "fetch_json",
                               return_value=table), \
             mock.patch.object(flake_report, "make_artifact_fetcher",
                               fake_fetcher):
            flaky, consistent, _, _ = flake_report.analyze_tab("dash", "tab", 3)
        findings = {f["test"]: f for f in flaky + consistent}
        # TestP: one real failure on a single changelist. Before the fix the
        # aborted rerun's pass counted as a retest flip, promoting it to a
        # reported flake; now it is correctly treated as that PR's own bug.
        self.assertNotIn("pkg.TestP", findings)
        # TestQ: its only flaky cell sits in the aborted column, so it must
        # not be reported at all.
        self.assertNotIn("pkg.TestQ", findings)

    def test_flaky_cell_is_a_test_story_not_pretest_breakage(self):
        # FLAKY_STATUS records a real failure (failed, then passed on a
        # rerun of the same column); a red run whose only failure is a
        # flaky cell must not be reported as pre-test PR breakage.
        table = {
            "query": "kubernetes-ci-logs/pr-logs/directory/job",
            "changelists": ["f0", "f1"],
            "column_ids": ["\ue000f0", "\ue000f1"],
            "timestamps": [200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 1])},
                {"name": "pkg.TestF", "statuses": rle([13, 1])},
            ],
        }
        art = {"f0": {"finished.json": {"result": "FAILURE",
                                        "revision": "abc"}}}

        def fake_fetcher(gcs_query, build_id):
            return lambda name: art.get(build_id, {}).get(name)

        with mock.patch.object(flake_report, "fetch_json",
                               return_value=table), \
             mock.patch.object(flake_report, "make_artifact_fetcher",
                               fake_fetcher):
            _, _, infra, _ = flake_report.analyze_tab("dash", "tab", 2)
        self.assertEqual(infra["pretest_failures"], 0)
        self.assertEqual(infra["infra_runs"], 0)

    def test_render_report_calls_out_benign_red_runs(self):
        flaky, consistent, infra = self.analyze()
        report = flake_report.render_report("dash", flaky, consistent, [infra], 6)
        self.assertIn("1 red run(s) were stale-PR clone failures (rebase needed)",
                      report)
        self.assertIn("pre-test PR breakage", report)
        self.assertIn("were aborted", report)
        self.assertIn("1 of 5 red runs", report)


class ShouldSkipForClosedTest(unittest.TestCase):
    # 2026-01-02T00:00:00Z in ms since epoch.
    CLOSED_AT = "2026-01-02T00:00:00Z"
    CLOSED_MS = 1767312000000

    def test_skips_when_last_failure_predates_close(self):
        self.assertTrue(flake_report.should_skip_for_closed(
            self.CLOSED_AT, self.CLOSED_MS - 1))

    def test_skips_when_last_failure_equals_close(self):
        # A failure at the close instant was visible to the closer.
        self.assertTrue(flake_report.should_skip_for_closed(
            self.CLOSED_AT, self.CLOSED_MS))

    def test_files_when_failure_is_newer_than_close(self):
        self.assertFalse(flake_report.should_skip_for_closed(
            self.CLOSED_AT, self.CLOSED_MS + 1))

    def test_files_when_close_time_missing_or_unparsable(self):
        # Without a provable close time the tool must not suppress.
        self.assertFalse(flake_report.should_skip_for_closed(None, 100))
        self.assertFalse(flake_report.should_skip_for_closed("", 100))
        self.assertFalse(flake_report.should_skip_for_closed("garbage", 100))


class FindClosedIssueTest(unittest.TestCase):
    MARKER = "<!-- flake-report:test=pkg.TestA -->"

    def test_matches_marker_only_not_title(self):
        issues = [
            {"number": 1, "title": "[FLAKE] TestA (tab)", "body": "no marker",
             "closedAt": "2026-01-05T00:00:00Z"},
            {"number": 2, "body": f"{self.MARKER}\nbody",
             "closedAt": "2026-01-01T00:00:00Z"},
        ]
        self.assertEqual(
            flake_report.find_closed_issue(issues, self.MARKER)["number"], 2)

    def test_picks_most_recently_closed_match(self):
        issues = [
            {"number": 1, "body": f"{self.MARKER}",
             "closedAt": "2026-01-01T00:00:00Z"},
            {"number": 2, "body": f"{self.MARKER}",
             "closedAt": "2026-02-01T00:00:00Z"},
        ]
        self.assertEqual(
            flake_report.find_closed_issue(issues, self.MARKER)["number"], 2)

    def test_none_when_no_match(self):
        self.assertIsNone(flake_report.find_closed_issue([], self.MARKER))


class UpdateIssuesClosedDedupTest(unittest.TestCase):
    """update_issues with a stubbed gh: closed issues suppress re-filing
    until a failure newer than the close appears."""

    CLOSED_AT = "2026-01-02T00:00:00Z"
    CLOSED_MS = 1767312000000

    def gh_stub(self, closed_issues):
        calls = []

        def fake_gh(*args, input_text=None):
            calls.append(args)
            if args[:2] == ("issue", "list"):
                state = args[args.index("--state") + 1]
                return json.dumps(closed_issues if state == "closed" else [])
            return ""

        return fake_gh, calls

    def flaky_finding(self, last_failure_ts):
        return {
            "test": "pkg.TestA", "short": "TestA", "tabs": ["tab"],
            "fails": 3, "passes": 7, "retest_flips": 1, "flaky_cells": 0,
            "distinct_changelists": 3, "last_failure_ts": last_failure_ts,
            "job_histories": ["https://prow.k8s.io/job-history/x"],
        }

    def infra_finding(self, last_failure_ts):
        return {
            "tab": "tab", "red_runs": 4, "infra_runs": 2, "aborted_runs": 0,
            "clone_failures": 0, "pretest_failures": 0, "total_runs": 9,
            "last_failure_ts": last_failure_ts,
            "job_history": "https://prow.k8s.io/job-history/x",
        }

    def closed_issue(self, marker):
        return [{"number": 42, "body": f"{marker}\nold body",
                 "closedAt": self.CLOSED_AT}]

    def test_closed_issue_suppresses_refile_of_stale_failures(self):
        marker = "<!-- flake-report:test=pkg.TestA -->"
        fake_gh, calls = self.gh_stub(self.closed_issue(marker))
        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [self.flaky_finding(self.CLOSED_MS - 1000)],
                [], dry_run=False)
        self.assertEqual(
            actions,
            ["skip (closed #42, no failures since close): TestA"])
        self.assertNotIn("create", {c[1] for c in calls})

    def test_newer_failure_refiles_and_links_prior_issue(self):
        marker = "<!-- flake-report:test=pkg.TestA -->"
        fake_gh, calls = self.gh_stub(self.closed_issue(marker))
        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [self.flaky_finding(self.CLOSED_MS + 1000)],
                [], dry_run=False)
        self.assertEqual(actions, ["create: [FLAKE] TestA"])
        (create,) = [c for c in calls if c[:2] == ("issue", "create")]
        body = create[create.index("--body") + 1]
        self.assertIn("Previously tracked in #42", body)
        self.assertIn(marker, body)

    def test_no_closed_match_files_as_before(self):
        fake_gh, calls = self.gh_stub([])
        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [self.flaky_finding(500)], [], dry_run=False)
        self.assertEqual(actions, ["create: [FLAKE] TestA"])
        (create,) = [c for c in calls if c[:2] == ("issue", "create")]
        self.assertNotIn("Previously tracked", create[create.index("--body") + 1])

    def test_infra_closed_issue_suppresses_refile(self):
        marker = "<!-- flake-report:infra-tab=tab -->"
        fake_gh, calls = self.gh_stub(self.closed_issue(marker))
        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [], [self.infra_finding(self.CLOSED_MS - 1000)],
                dry_run=False)
        self.assertEqual(
            actions,
            ["skip (closed #42, no failures since close): infra tab"])
        self.assertNotIn("create", {c[1] for c in calls})

    def test_infra_newer_failure_refiles_with_lineage(self):
        marker = "<!-- flake-report:infra-tab=tab -->"
        fake_gh, calls = self.gh_stub(self.closed_issue(marker))
        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [], [self.infra_finding(self.CLOSED_MS + 1000)],
                dry_run=False)
        self.assertEqual(
            actions,
            ["create: [FLAKE] tab: infra failures before tests ran"])
        (create,) = [c for c in calls if c[:2] == ("issue", "create")]
        self.assertIn("Previously tracked in #42",
                      create[create.index("--body") + 1])

    def test_open_issue_still_takes_precedence_over_closed(self):
        # An open issue for the marker means the closed-issue logic never
        # runs: the open issue is updated (or skipped) exactly as before.
        marker = "<!-- flake-report:test=pkg.TestA -->"
        open_issue = [{
            "number": 7, "title": "[FLAKE] TestA (tab)",
            "body": f"{marker}\n<!-- last-reported-failure=1 -->",
        }]
        calls = []

        def fake_gh(*args, input_text=None):
            calls.append(args)
            if args[:2] == ("issue", "list"):
                state = args[args.index("--state") + 1]
                return json.dumps(
                    self.closed_issue(marker) if state == "closed"
                    else open_issue)
            return ""

        with mock.patch.object(flake_report, "gh", fake_gh):
            actions = flake_report.update_issues(
                "org/repo", [self.flaky_finding(self.CLOSED_MS - 1000)],
                [], dry_run=False)
        self.assertEqual(actions, ["update #7: TestA"])


if __name__ == "__main__":
    unittest.main()


class FalsePositiveFilterTest(unittest.TestCase):
    """The four filters that keep a PR's own breakage out of flake counts."""

    QUERY = "kubernetes-ci-logs/pr-logs/directory/job"

    def analyze(self, table, artifacts, pr_files=None, dirs=None):
        fetched = []

        def fake_fetcher(gcs_query, build_id):
            def fetch(name):
                fetched.append((build_id, name))
                return artifacts.get(build_id, {}).get(name)
            fetch.resolved_dir = lambda: (dirs or {}).get(build_id)
            return fetch

        with mock.patch.object(flake_report, "fetch_json", return_value=table), \
             mock.patch.object(flake_report, "make_artifact_fetcher", fake_fetcher):
            result = flake_report.analyze_tab("dash", "tab", 10, pr_files)
        return result, fetched

    def test_aborted_run_with_green_overall_does_not_count(self):
        # Column 0: run aborted mid-suite; Overall never went red but TestA
        # carries the junit error. Column 1: clean pass with no failing cell
        # (must not cost an artifact fetch). Column 2: a real failure.
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2"],
            "column_ids": ["b0", "b1", "b2"],
            "timestamps": [300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([1, 1, 12])},
                {"name": "pkg.TestA", "statuses": rle([12, 1, 12])},
            ],
        }
        artifacts = {"b0": {"finished.json": {"result": "aborted"}},
                     "b2": {"finished.json": {"result": "FAILURE", "revision": "x"}}}
        (flaky, consistent, infra, pr_local), fetched = self.analyze(table, artifacts)
        self.assertEqual(flaky, [])
        # One failing column on one changelist with a pass in the window is
        # neither flaky nor consistent; nothing is reported.
        self.assertEqual(consistent, [])
        self.assertEqual(pr_local, [])
        self.assertNotIn("b1", {b for b, _ in fetched})
        # Infra accounting still counts only red-Overall columns.
        self.assertEqual(infra["red_runs"], 1)
        self.assertEqual(infra["aborted_runs"], 0)

    def test_suite_wide_failure_is_excluded_per_suite(self):
        # 80 Go rows and 10 pytest rows. Column 0 fails every pytest row
        # (100% of that suite, 11% of the lane); column 1 fails 2 Go rows.
        # Columns 2-3 are green so the Go rows look flaky across two PRs.
        go = [{"name": f"pkg.TestGo{i}", "statuses": rle([1, 12 if i < 2 else 1, 1, 1])}
              for i in range(80)]
        py = [{"name": f"pytest.test_py{i}", "statuses": rle([12, 1, 1, 1])}
              for i in range(10)]
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2", "b3"],
            "column_ids": ["b0", "b1", "b2", "b3"],
            "timestamps": [400, 300, 200, 100],
            "tests": [{"name": "job.Overall", "statuses": rle([12, 12, 1, 1])}] + go + py,
        }
        artifacts = {b: {"finished.json": {"result": "FAILURE", "revision": "x"}}
                     for b in ("b0", "b1")}
        dirs = {"b0": "https://x/pr-logs/pull/o_r/10/job/b0",
                "b1": "https://x/pr-logs/pull/o_r/11/job/b1"}
        (flaky, consistent, _, pr_local), _ = self.analyze(table, artifacts, dirs=dirs)
        names = {f["test"] for f in flaky}
        self.assertFalse(any(n.startswith("pytest.") for n in names), names)
        self.assertEqual(consistent, [])
        self.assertEqual(pr_local, [])
        # The Go rows failing in column 1 only ever failed on one PR, so
        # they are not flaky either; but they were counted (not excluded).
        self.assertEqual(names, set())

    def test_setup_errors_across_a_suite_are_a_harness_failure(self):
        # Three pytest rows, two of which fail in column 0 with fixture
        # setup errors; under the size threshold, but the messages say the
        # harness broke, not the tests.
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2"],
            "column_ids": ["b0", "b1", "b2"],
            "timestamps": [300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 1, 1])},
                {"name": "pytest.test_a", "statuses": rle([12, 1, 1]),
                 "messages": ['failed on setup with "TimeoutError"', "", ""]},
                {"name": "pytest.test_b", "statuses": rle([12, 1, 1]),
                 "messages": ['failed on setup with "TimeoutError"', "", ""]},
                {"name": "pytest.test_c", "statuses": rle([1, 1, 1])},
            ],
        }
        artifacts = {"b0": {"finished.json": {"result": "FAILURE", "revision": "x"}}}
        (flaky, consistent, _, pr_local), _ = self.analyze(table, artifacts)
        self.assertEqual((flaky, consistent, pr_local), ([], [], []))

    def test_pr_touching_test_sources_is_not_a_flake(self):
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2", "b3", "b4"],
            "column_ids": ["b0", "b1", "b2", "b3", "b4"],
            "timestamps": [500, 400, 300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 12, 1, 1, 1])},
                {"name": "pytest.test_x", "statuses": rle([12, 12, 1, 1, 1])},
            ],
        }
        artifacts = {b: {"finished.json": {"result": "FAILURE", "revision": "x"}}
                     for b in ("b0", "b1")}
        dirs = {"b0": "https://x/pr-logs/pull/o_r/4242/job/b0",
                "b1": "https://x/pr-logs/pull/o_r/4343/job/b1"}
        calls = []

        def pr_files(pr):
            calls.append(pr)
            return {"4242": ["test/e2e/clients/python/conftest.py"],
                    "4343": ["README.md"]}[pr]

        (flaky, consistent, _, pr_local), _ = self.analyze(
            table, artifacts, pr_files=pr_files, dirs=dirs)
        # 4242 touched the harness: excluded. 4343 did not: counted. One
        # genuine failure on one PR with a pass is not flaky.
        self.assertEqual(flaky, [])
        self.assertEqual(consistent, [])
        self.assertEqual(pr_local, [])
        self.assertEqual(sorted(calls), ["4242", "4343"])

        # Both PRs touching the harness: everything is PR-local.
        (flaky, consistent, _, pr_local), _ = self.analyze(
            table, artifacts, dirs=dirs,
            pr_files=lambda pr: ["test/e2e/clients/python/conftest.py"])
        self.assertEqual(flaky, [])
        self.assertEqual([p["reason"] for p in pr_local], ["pr-touches-test-sources"])
        self.assertEqual(pr_local[0]["runs"], 2)

        # Without a lookup (--no-pr-files) both failures count and two
        # distinct PRs make it flaky; the counter stays at zero.
        (flaky, _, _, _), _ = self.analyze(table, artifacts, dirs=dirs)
        (finding,) = flaky
        self.assertEqual(finding["self_regression_cols"], 0)
        self.assertEqual(finding["distinct_changelists"], 2)

    def test_distinct_changelists_counts_prs_not_builds(self):
        # Three pushes of the same PR each failed TestA; a fourth column
        # from another PR passed. Build IDs differ, the PR does not.
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2", "b3"],
            "column_ids": ["b0", "b1", "b2", "b3"],
            "timestamps": [400, 300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 12, 12, 1])},
                {"name": "pkg.TestA", "statuses": rle([12, 12, 12, 1])},
                {"name": "pkg.TestB", "statuses": rle([1, 1, 1, 1])},
            ],
        }
        artifacts = {b: {"finished.json": {"result": "FAILURE", "revision": "x"}}
                     for b in ("b0", "b1", "b2")}
        dirs = {b: "https://x/pr-logs/pull/o_r/77/job/" + b for b in ("b0", "b1", "b2")}
        (flaky, consistent, _, pr_local), _ = self.analyze(table, artifacts, dirs=dirs)
        self.assertEqual(flaky, [])
        # Three pushes of one PR: PR-local, not "failing at head", even
        # though the test passed on another PR earlier in the window.
        self.assertEqual(consistent, [])
        self.assertEqual([(p["test"], p["reason"]) for p in pr_local],
                         [("pkg.TestA", "failing-only-on-one-pr")])

    def test_consistent_requires_history(self):
        table = {
            "query": self.QUERY,
            "changelists": ["b0", "b1", "b2"],
            "column_ids": ["b0", "b1", "b2"],
            "timestamps": [300, 200, 100],
            "tests": [
                {"name": "job.Overall", "statuses": rle([12, 12, 1])},
                # Failing at head across two PRs, never passed: breakage.
                {"name": "pkg.TestBroken", "statuses": rle([12, 12, 0])},
                # Exists only on one PR's run: that PR's new test.
                {"name": "pkg.TestNew", "statuses": rle([12, 0, 0])},
            ],
        }
        artifacts = {b: {"finished.json": {"result": "FAILURE", "revision": "x"}}
                     for b in ("b0", "b1")}
        dirs = {"b0": "https://x/pr-logs/pull/o_r/1/job/b0",
                "b1": "https://x/pr-logs/pull/o_r/2/job/b1"}
        (flaky, consistent, _, pr_local), _ = self.analyze(table, artifacts, dirs=dirs)
        self.assertEqual([f["test"] for f in consistent], ["pkg.TestBroken"])
        self.assertEqual([p["test"] for p in pr_local], ["pkg.TestNew"])
        report = flake_report.render_report("dash", flaky, consistent, [], 3, pr_local)
        self.assertIn("PR-local failures", report)
        self.assertIn("pkg.TestNew", report)


class HelperTest(unittest.TestCase):
    def test_suite_of(self):
        self.assertEqual(flake_report.suite_of("pytest.test_x"), "pytest")
        self.assertEqual(flake_report.suite_of(
            "src/__tests__/tunnel.test.ts.PodTunnel > relays bytes"),
            "src/__tests__/tunnel.test.ts")
        self.assertEqual(flake_report.suite_of(
            "test-e2e-typescript-sdk.test.ts.Sandbox > creates"),
            "test-e2e-typescript-sdk.test.ts")
        self.assertEqual(flake_report.suite_of(
            "sigs.k8s.io/agent-sandbox/test/e2e/extensions.TestFoo/sub"),
            "sigs.k8s.io/agent-sandbox/test/e2e/extensions")
        self.assertEqual(flake_report.suite_of("pkg.TestA"), "pkg")
        self.assertEqual(flake_report.suite_of("weird"), "other")

    def test_test_source_paths(self):
        self.assertIn("test/e2e/clients/python/",
                      flake_report.test_source_paths("pytest.test_x"))
        self.assertIn("clients/typescript/agentic-sandbox-client/src/__tests__/tunnel.test.ts",
                      flake_report.test_source_paths("src/__tests__/tunnel.test.ts.PodTunnel > x"))
        self.assertEqual(flake_report.test_source_paths(
            "sigs.k8s.io/agent-sandbox/test/e2e/extensions.TestFoo"),
            ["test/e2e/extensions/"])
        self.assertEqual(flake_report.test_source_paths("weird"), [])

    def test_touches(self):
        self.assertTrue(flake_report.touches(["test/e2e/clients/python/conftest.py"],
                                             ["test/e2e/clients/python/"]))
        self.assertTrue(flake_report.touches(["a/b.ts"], ["a/b.ts"]))
        self.assertFalse(flake_report.touches(["README.md"], ["test/e2e/clients/python/"]))
        self.assertFalse(flake_report.touches(None, ["x/"]))

    def test_pr_number_from_dir(self):
        self.assertEqual(flake_report.pr_number_from_dir(
            "https://storage.googleapis.com/kubernetes-ci-logs/pr-logs/pull/"
            "kubernetes-sigs_agent-sandbox/1827/presubmit-agent-sandbox-test-e2e/210"),
            "1827")
        self.assertIsNone(flake_report.pr_number_from_dir(
            "https://storage.googleapis.com/kubernetes-ci-logs/logs/periodic/210"))
        self.assertIsNone(flake_report.pr_number_from_dir(None))

    def test_align_messages(self):
        # Aligned when lengths match.
        self.assertEqual(flake_report.align_messages([12, 1, 0], ["a", "", "c"]),
                         {0: "a", 2: "c"})
        # Compressed to non-empty cells otherwise.
        self.assertEqual(flake_report.align_messages([0, 12, 0, 1], ["x", "y"]),
                         {1: "x", 3: "y"})
        self.assertEqual(flake_report.align_messages([12, 12], None), {})

    def test_make_pr_files_lookup_caches_and_tolerates_failure(self):
        calls = []

        def fake_gh(*args, input_text=None):
            calls.append(args)
            if "9" in args:
                raise RuntimeError("boom")
            return "a.py\nb.py\n"

        with mock.patch.object(flake_report, "gh", fake_gh):
            lookup = flake_report.make_pr_files_lookup("o/r")
            self.assertEqual(lookup("8"), ["a.py", "b.py"])
            self.assertEqual(lookup("8"), ["a.py", "b.py"])
            self.assertIsNone(lookup("9"))
        self.assertEqual(len(calls), 2)

    def test_make_pr_files_lookup_without_gh_binary(self):
        # Report-only mode on a machine without gh: the lookup must degrade
        # to "unknown", not raise and skip the tab.
        with mock.patch.object(flake_report, "gh",
                               side_effect=FileNotFoundError("gh")):
            lookup = flake_report.make_pr_files_lookup("o/r")
            self.assertIsNone(lookup("8"))
