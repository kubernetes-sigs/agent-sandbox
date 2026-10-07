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

"""Unit tests for the unit test runner."""

import importlib.util
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)
_loader = SourceFileLoader("test_unit", os.path.join(_SCRIPT_DIR, "test-unit"))
_spec = importlib.util.spec_from_loader("test_unit", _loader)
test_unit = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(test_unit)


class RunGoTestsTest(unittest.TestCase):
    def test_includes_framework_without_running_cluster_tests(self):
        """Verify framework inclusion and E2E exclusion across Go modules."""
        module = "sigs.k8s.io/agent-sandbox"
        repo_root = os.path.dirname(os.path.dirname(_SCRIPT_DIR))
        artifact_dir = os.path.join(repo_root, "bin")
        packages = [
            f"{module}/controllers",
            f"{module}/test/e2e",
            f"{module}/test/e2e/extensions",
            f"{module}/test/e2e/clients/python",
            f"{module}/test/e2e/framework",
            f"{module}/test/e2e/framework/predicates",
            f"{module}/test/e2e/framework-extra",
        ]
        with (
            mock.patch.object(test_unit.subprocess, "check_output", side_effect=[
                "go.mod\ndev/tools/go.mod\n",
                "\n".join(packages) + "\n",
                f"{module}/dev/tools/mdtoc\n",
            ]),
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.return_value.returncode = 0
            self.assertEqual(test_unit.run_go_tests(repo_root, artifact_dir), 0)

        self.assertEqual(run.call_args_list, [
            mock.call(test_unit.utils.go_tool_args(
                "gotestsum",
                f"--junitfile={os.path.join(artifact_dir, 'junit_unit-go.xml')}",
                "--", "-race",
                f"{module}/controllers",
                f"{module}/test/e2e/framework",
                f"{module}/test/e2e/framework/predicates",
            ), cwd=repo_root),
            mock.call(test_unit.utils.go_tool_args(
                "gotestsum",
                f"--junitfile={os.path.join(artifact_dir, 'junit_unit-go-dev-tools.xml')}",
                "--", "-race", f"{module}/dev/tools/mdtoc",
            ), cwd=os.path.join(repo_root, "dev", "tools")),
        ])


class RunTypeScriptTestsTest(unittest.TestCase):
    def test_runs_static_checks_before_unit_tests(self):
        repo_root = "/repo"
        artifact_dir = "/artifacts"
        npm_path = "/tools/npm"
        npx_path = "/tools/npx"

        with (
            mock.patch.object(test_unit.os.path, "isdir", return_value=True),
            mock.patch.object(
                test_unit, "ensure_node", return_value=(npm_path, npx_path)),
            mock.patch.object(test_unit.subprocess, "check_call") as check_call,
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.return_value.returncode = 0
            self.assertEqual(
                test_unit.run_typescript_tests(repo_root, artifact_dir), 0)

        ts_dir = os.path.join(
            repo_root, "clients", "typescript", "agentic-sandbox-client")
        check_call.assert_called_once_with(
            [npm_path, "install", "--prefer-offline"],
            cwd=ts_dir,
            env=mock.ANY,
        )
        self.assertEqual(run.call_args_list, [
            mock.call(
                [npm_path, "run", "check"], cwd=ts_dir, env=mock.ANY),
            mock.call(
                [npm_path, "run", "typecheck"], cwd=ts_dir, env=mock.ANY),
            mock.call([
                npx_path, "vitest", "run",
                "--reporter=verbose",
                "--reporter=junit",
                "--outputFile.junit=/artifacts/junit_unit-typescript.xml",
            ], cwd=ts_dir, env=mock.ANY),
        ])

    def test_propagates_static_check_failure_after_running_all_checks(self):
        with (
            mock.patch.object(test_unit.os.path, "isdir", return_value=True),
            mock.patch.object(
                test_unit, "ensure_node", return_value=("npm", "npx")),
            mock.patch.object(test_unit.subprocess, "check_call"),
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.side_effect = [
                mock.Mock(returncode=1),
                mock.Mock(returncode=0),
                mock.Mock(returncode=0),
            ]
            self.assertEqual(
                test_unit.run_typescript_tests("/repo", "/artifacts"), 1)
            self.assertEqual(run.call_count, 3)


if __name__ == "__main__":
    unittest.main()
