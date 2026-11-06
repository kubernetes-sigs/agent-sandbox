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

"""Unit tests for the extensionless dev/tools/pr-failure-summary script."""

import importlib.util
import json
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_SCRIPT_PATH = os.path.join(_SCRIPT_DIR, "pr-failure-summary")
_loader = SourceFileLoader("pr_failure_summary", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("pr_failure_summary", _loader)
pr_failure_summary = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(pr_failure_summary)


class KnownFlakesTest(unittest.TestCase):
    def test_lane_level_issues_do_not_become_test_keys(self):
        issues = [
            {"number": 1, "title": "[FLAKE] TestA (tab)",
             "body": "<!-- flake-report:test=pkg.TestA -->"},
            {"number": 2, "title": "[FLAKE] TestB", "body": "filed by hand"},
            {"number": 3, "title": "[FLAKE] presubmit-test-e2e: infrastructure failures before tests ran",
             "body": "<!-- flake-report:infra-tab=presubmit-test-e2e -->"},
            {"number": 4, "title": "[FLAKE] post-agent-sandbox-push-images: job fails intermittently",
             "body": "<!-- flake-report:job-flake=post-agent-sandbox-push-images -->"},
            {"number": 5, "title": "[FLAKE] some-job: hand-filed lane issue", "body": ""},
        ]
        with mock.patch.object(pr_failure_summary, "gh", return_value=json.dumps(issues)):
            self.assertEqual(pr_failure_summary.known_flakes("o/r"),
                             {"pkg.TestA": 1, "TestB": 2})


if __name__ == "__main__":
    unittest.main()
