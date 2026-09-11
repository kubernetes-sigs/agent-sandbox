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

    def test_clone_record_failed_is_a_clone_failure(self):
        cls = flake_report.classify_red_run(
            False, False,
            artifacts(
                finished={"result": "FAILURE"},
                clone_records=[{"refs": {"repo": ""}},
                               {"refs": {"repo": "agent-sandbox"}, "failed": True}],
            ))
        self.assertEqual(cls, "clone_failure")

    def test_missing_clone_records_with_no_revision_is_a_clone_failure(self):
        # finished.json only records "revision" when the checkout succeeded.
        cls = flake_report.classify_red_run(
            False, False, artifacts(finished={"result": "FAILURE"}))
        self.assertEqual(cls, "clone_failure")

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
               "clone-records.json": [{"failed": True}]},
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
            return flake_report.analyze_tab("dash", "tab", 6)

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

    def test_render_report_calls_out_benign_red_runs(self):
        flaky, consistent, infra = self.analyze()
        report = flake_report.render_report("dash", flaky, consistent, [infra], 6)
        self.assertIn("1 red run(s) were stale-PR clone failures (rebase needed)",
                      report)
        self.assertIn("pre-test PR breakage", report)
        self.assertIn("were aborted", report)
        self.assertIn("1 of 5 red runs", report)


if __name__ == "__main__":
    unittest.main()
