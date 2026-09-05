#!/usr/bin/env python3
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

"""Unit tests for dev/ci/periodics/test-testgrid-failure-issues.

Runs entirely offline: the GitHub REST API is replaced by an in-memory
fake, and TestGrid's summary endpoint is stubbed per test. Nothing here
talks to a network or writes a real issue.
"""

import importlib.util
import os
import re
import sys
import unittest
from importlib.machinery import SourceFileLoader

# The script is an extensionless file, so load it via importlib rather than
# a normal import (same pattern as dev/tools/push_images_test.py).
_PERIODICS_DIR = os.path.dirname(os.path.abspath(__file__))
_SCRIPT_PATH = os.path.join(_PERIODICS_DIR, "test-testgrid-failure-issues")
_loader = SourceFileLoader("testgrid_failure_issues", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("testgrid_failure_issues", _loader)
tgfi = importlib.util.module_from_spec(_spec)
_loader.exec_module(tgfi)


class FakeGitHub:
    """In-memory stand-in for the GitHub REST calls the script makes.

    The list-issues call is paginated the same way the real endpoint is
    (bounded by per_page, more pages while a page comes back full), so
    tests can exercise find_open_tracking_issue's pagination loop directly
    instead of trusting it by inspection.
    """

    def __init__(self):
        self.issues = []
        self._next_number = 1

    def request(self, method, url, token, body=None):
        if method == "GET" and "/issues?" in url:
            # "[?&]" (not a bare substring match) so this doesn't match the
            # "page=100" tail of "per_page=100" and misread the page number.
            page = int(re.search(r"[?&]page=(\d+)", url).group(1))
            per_page = int(re.search(r"per_page=(\d+)", url).group(1))
            open_issues = [i for i in self.issues if i["state"] == "open"]
            start = (page - 1) * per_page
            return open_issues[start : start + per_page]
        if method == "POST" and url.endswith("/issues"):
            issue = {
                "number": self._next_number,
                "title": body["title"],
                "body": body["body"],
                "labels": body.get("labels", []),
                "state": "open",
                "comments": [],
            }
            self._next_number += 1
            self.issues.append(issue)
            return issue
        if method == "POST" and "/comments" in url:
            number = int(re.search(r"/issues/(\d+)/comments", url).group(1))
            self._get(number)["comments"].append(body["body"])
            return {}
        if method == "PATCH" and re.search(r"/issues/\d+$", url):
            number = int(re.search(r"/issues/(\d+)$", url).group(1))
            self._get(number)["state"] = body["state"]
            return {}
        raise AssertionError(f"unexpected GitHub call: {method} {url}")

    def _get(self, number):
        return next(i for i in self.issues if i["number"] == number)


DASHBOARD = "sig-apps-agent-sandbox"
REPO = "kubernetes-sigs/agent-sandbox"
TAB = "periodic-agent-sandbox-migration-test"

FAILING = {
    "overall_status": "FAILING",
    "status": "3 of 10 recent runs failing",
    "tests": [{"display_name": "pkg.TestThing", "failure_message": "boom\nstack..."}],
}
PASSING = {"overall_status": "PASSING", "status": "10 of 10 recent runs passing", "tests": []}
FLAKY = {"overall_status": "FLAKY", "status": "7 of 10 recent runs passing", "tests": []}
STALE = {"overall_status": "STALE", "status": "no recent results", "tests": []}


class HandleTabTest(unittest.TestCase):
    def setUp(self):
        self.github = FakeGitHub()
        self._orig_request = tgfi._request
        tgfi._request = self.github.request

    def tearDown(self):
        tgfi._request = self._orig_request

    def _handle(self, tab_data, dry_run=False):
        tgfi.handle_tab(REPO, "tok", DASHBOARD, TAB, tab_data, dry_run=dry_run)

    def test_files_issue_on_first_failure(self):
        self._handle(FAILING)
        self.assertEqual(len(self.github.issues), 1)
        issue = self.github.issues[0]
        self.assertIn(TAB, issue["title"])
        self.assertEqual(issue["labels"], [tgfi.LABEL])
        self.assertIn(
            tgfi.MARKER_TEMPLATE.format(dashboard=DASHBOARD, tab=TAB), issue["body"]
        )

    def test_does_not_duplicate_across_repeated_failures(self):
        self._handle(FAILING)
        self._handle(FAILING)
        self._handle(FAILING)
        self.assertEqual(
            len(self.github.issues), 1,
            "a persisting failure must not file more than one issue",
        )

    def test_closes_on_passing(self):
        self._handle(FAILING)
        self._handle(PASSING)
        issue = self.github.issues[0]
        self.assertEqual(issue["state"], "closed")
        self.assertEqual(len(issue["comments"]), 1)

    def test_flaky_does_not_close_open_issue(self):
        self._handle(FAILING)
        self._handle(FLAKY)
        self.assertEqual(self.github.issues[0]["state"], "open")

    def test_stale_does_not_close_open_issue(self):
        self._handle(FAILING)
        self._handle(STALE)
        self.assertEqual(
            self.github.issues[0]["state"], "open",
            "STALE can mean the job stopped reporting results -- not a fix",
        )

    def test_files_fresh_issue_after_prior_one_closed(self):
        self._handle(FAILING)
        self._handle(PASSING)
        self._handle(FAILING)
        self.assertEqual(len(self.github.issues), 2)
        self.assertEqual(self.github.issues[0]["state"], "closed")
        self.assertEqual(self.github.issues[1]["state"], "open")

    def test_dry_run_makes_no_writes(self):
        self._handle(FAILING, dry_run=True)
        self.assertEqual(self.github.issues, [])

    def test_unrelated_issue_is_not_mistaken_for_tracking_issue(self):
        # An open issue that doesn't contain this tab's exact marker (e.g.
        # another tab's tracking issue) must not be treated as this tab's.
        self.github.issues.append({
            "number": 99,
            "state": "open",
            "title": "unrelated",
            "body": "mentions testgrid-failure but not the real marker",
            "comments": [],
        })
        self._handle(FAILING)
        self.assertEqual(len(self.github.issues), 2)
        self.assertEqual(self.github.issues[0]["state"], "open")  # untouched
        new_issue = self.github.issues[1]
        self.assertIn(
            tgfi.MARKER_TEMPLATE.format(dashboard=DASHBOARD, tab=TAB), new_issue["body"]
        )

    def test_finds_existing_issue_past_first_page(self):
        # 150 other open issues push this tab's existing tracking issue onto
        # the lookup's second page (per_page=100). The lookup must keep
        # paginating and find it, rather than stopping after page one and
        # wrongly concluding no tracking issue exists (which would file a
        # duplicate).
        for i in range(150):
            self.github.issues.append({
                "number": i,
                "state": "open",
                "title": "other tab",
                "body": f"<!-- testgrid-failure:{DASHBOARD}/other-tab-{i} -->",
                "comments": [],
            })
        real_marker = tgfi.MARKER_TEMPLATE.format(dashboard=DASHBOARD, tab=TAB)
        self.github.issues.append({
            "number": 150,
            "state": "open",
            "title": f"[TestGrid] {TAB} is failing",
            "body": real_marker,
            "comments": [],
        })

        self._handle(FAILING)
        self.assertEqual(
            len(self.github.issues), 151, "existing issue on page 2 must be found, not duplicated"
        )

        self._handle(PASSING)
        self.assertEqual(self.github.issues[150]["state"], "closed")


class IssueBodyTest(unittest.TestCase):
    def test_truncates_long_failure_message(self):
        long_line = "x" * 500
        data = {
            "overall_status": "FAILING",
            "status": "s",
            "tests": [{"display_name": "t", "failure_message": long_line}],
        }
        body = tgfi.issue_body(DASHBOARD, TAB, data, "MARK")
        self.assertIn("x" * tgfi.MAX_FAILURE_MESSAGE_LENGTH + "...", body)
        self.assertNotIn(long_line, body)

    def test_lists_up_to_max_failing_tests_and_notes_remainder(self):
        tests = [{"display_name": f"t{i}", "failure_message": ""} for i in range(8)]
        data = {"overall_status": "FAILING", "status": "s", "tests": tests}
        body = tgfi.issue_body(DASHBOARD, TAB, data, "MARK")
        for i in range(tgfi.MAX_FAILING_TESTS_LISTED):
            self.assertIn(f"`t{i}`", body)
        self.assertIn("...and 3 more", body)


class MainSkipsPresubmitsTest(unittest.TestCase):
    def setUp(self):
        self.github = FakeGitHub()
        self._orig_request = tgfi._request
        self._orig_fetch = tgfi.fetch_summary
        self._orig_token = tgfi._token
        self._orig_dashboards = tgfi.DASHBOARD_REPOS
        self._orig_argv = sys.argv
        tgfi._request = self.github.request
        tgfi._token = lambda: "tok"
        tgfi.DASHBOARD_REPOS = [(DASHBOARD, REPO)]
        sys.argv = ["test-testgrid-failure-issues"]

    def tearDown(self):
        tgfi._request = self._orig_request
        tgfi.fetch_summary = self._orig_fetch
        tgfi._token = self._orig_token
        tgfi.DASHBOARD_REPOS = self._orig_dashboards
        sys.argv = self._orig_argv

    def test_presubmit_and_pull_tabs_are_never_actioned(self):
        tgfi.fetch_summary = lambda dashboard: {
            "presubmit-test-unit": FAILING,
            "pull-something": FAILING,
            TAB: FAILING,
        }
        rc = tgfi.main()
        self.assertEqual(rc, 0)
        # Only the one non-presubmit, non-pull tab should file an issue.
        self.assertEqual(len(self.github.issues), 1)
        self.assertIn(TAB, self.github.issues[0]["title"])

    def test_dashboard_fetch_error_is_isolated_and_reported(self):
        def failing_fetch(dashboard):
            raise tgfi.urllib.error.URLError("boom")

        tgfi.DASHBOARD_REPOS = [(DASHBOARD, REPO), ("other-dashboard", REPO)]
        tgfi.fetch_summary = failing_fetch
        rc = tgfi.main()
        self.assertEqual(rc, 1)
        self.assertEqual(self.github.issues, [])


if __name__ == "__main__":
    unittest.main()
