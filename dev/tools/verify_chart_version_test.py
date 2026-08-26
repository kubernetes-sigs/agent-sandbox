# Copyright 2025 The Kubernetes Authors.
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

"""Unit tests for verify-chart-version, against throwaway git repositories."""

import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path
from unittest import mock

# Load the extensionless verify-chart-version script via importlib (same
# pattern as dev/tools/push_images_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_loader = SourceFileLoader(
    "verify_chart_version", os.path.join(_SCRIPT_DIR, "verify-chart-version")
)
_spec = importlib.util.spec_from_loader("verify_chart_version", _loader)
verify_chart_version = importlib.util.module_from_spec(_spec)
_loader.exec_module(verify_chart_version)

CHART_TEMPLATE = """apiVersion: v2
name: agent-sandbox
description: Kubernetes controller for managing agent sandboxes
type: application
version: {version}
"""


class ChartVersionTestCase(unittest.TestCase):
    """Builds a repo with a base branch and a feature branch to diff against."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.repo = Path(self._tmp.name)

        # verify-chart-version reads GITHUB_BASE_REF/PULL_BASE_REF, and treats
        # CI as fatal-on-missing-base. Pin the environment for determinism.
        env_patch = mock.patch.dict(
            os.environ,
            {"GITHUB_BASE_REF": "main"},
            clear=False,
        )
        env_patch.start()
        self.addCleanup(env_patch.stop)
        for name in ("CI", "GITHUB_ACTIONS", "PROW_JOB_ID", "PULL_BASE_REF"):
            os.environ.pop(name, None)

        self.git("init", "--initial-branch=main")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "Test")

        self.write_chart("0.1.0")
        self.write("helm/templates/rbac.generated.yaml", "kind: ClusterRole\n")
        self.git("add", "-A")
        self.git("commit", "-m", "base")

        self.git("checkout", "-q", "-b", "feature")

    def git(self, *args):
        subprocess.run(
            ["git"] + list(args),
            cwd=self.repo,
            check=True,
            capture_output=True,
            text=True,
        )

    def write(self, relative_path, contents):
        path = self.repo / relative_path
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)

    def write_chart(self, version):
        self.write("helm/Chart.yaml", CHART_TEMPLATE.format(version=version))

    def change_generated_content(self):
        self.write(
            "helm/templates/rbac.generated.yaml",
            "kind: ClusterRole\n# a newly generated rule\n",
        )

    def verify(self):
        return verify_chart_version.verify(str(self.repo))


class TestVerifyChartVersion(ChartVersionTestCase):

    def test_passes_when_nothing_changed(self):
        self.assertEqual(0, self.verify())

    def test_passes_when_only_untracked_non_chart_files_exist(self):
        self.write("some-scratch-file.txt", "ignored\n")
        self.assertEqual(0, self.verify())

    def test_fails_when_generated_content_changed_without_a_bump(self):
        self.change_generated_content()
        self.assertEqual(1, self.verify())

    def test_passes_when_generated_content_changed_with_a_patch_bump(self):
        self.change_generated_content()
        self.write_chart("0.1.1")
        self.assertEqual(0, self.verify())

    def test_passes_when_generated_content_changed_with_a_minor_bump(self):
        self.change_generated_content()
        self.write_chart("0.2.0")
        self.assertEqual(0, self.verify())

    def test_fails_when_the_version_is_lowered(self):
        self.change_generated_content()
        self.write_chart("0.0.9")
        self.assertEqual(1, self.verify())

    def test_fails_when_chart_yaml_changed_but_version_did_not(self):
        # #620's check only asked whether Chart.yaml changed at all, so editing
        # an unrelated field was enough to satisfy it.
        self.change_generated_content()
        self.write(
            "helm/Chart.yaml",
            CHART_TEMPLATE.format(version="0.1.0").replace(
                "Kubernetes controller", "A different description"
            ),
        )
        self.assertEqual(1, self.verify())

    def test_detects_untracked_generated_files(self):
        # git diff does not report untracked files; a brand new CRD would
        # otherwise slip through without a version bump.
        self.write("helm/crds/agents.x-k8s.io_sandboxes.yaml", "kind: CustomResourceDefinition\n")
        self.assertEqual(1, self.verify())

    def test_passes_when_committed_bump_accompanies_committed_content(self):
        self.change_generated_content()
        self.write_chart("0.1.1")
        self.git("add", "-A")
        self.git("commit", "-m", "regenerate chart and bump version")
        self.assertEqual(0, self.verify())

    def test_fails_when_chart_is_missing(self):
        (self.repo / "helm" / "Chart.yaml").unlink()
        self.assertEqual(1, self.verify())

    def test_passes_when_chart_is_new_in_this_change(self):
        # A branch that introduces the chart has nothing to bump from.
        self.git("rm", "-q", "helm/Chart.yaml")
        self.git("commit", "-m", "remove chart")
        self.git("checkout", "-q", "-b", "adds-chart")
        self.write_chart("0.1.0")
        self.change_generated_content()

        with mock.patch.dict(os.environ, {"GITHUB_BASE_REF": "feature"}):
            self.assertEqual(0, self.verify())

    def test_fails_in_ci_when_no_comparison_base_exists(self):
        with mock.patch.dict(
            os.environ, {"GITHUB_BASE_REF": "no-such-branch", "CI": "true"}
        ):
            self.assertEqual(1, self.verify())

    def test_skips_outside_ci_when_no_comparison_base_exists(self):
        with mock.patch.dict(os.environ, {"GITHUB_BASE_REF": "no-such-branch"}):
            self.assertEqual(0, self.verify())


if __name__ == "__main__":
    unittest.main()
