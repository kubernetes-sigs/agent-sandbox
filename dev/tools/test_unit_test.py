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

import contextlib
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
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.return_value.returncode = 0
            self.assertEqual(
                test_unit.run_typescript_tests(
                    repo_root, artifact_dir, {}, npm_path, npx_path), 0)

        ts_dir = os.path.join(
            repo_root, "clients", "typescript", "agentic-sandbox-client")
        self.assertEqual(run.call_args_list, [
            mock.call(
                [npm_path, "install", "--prefer-offline"], cwd=ts_dir, env=mock.ANY),
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
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.side_effect = [
                mock.Mock(returncode=0),
                mock.Mock(returncode=1),
                mock.Mock(returncode=0),
                mock.Mock(returncode=0),
            ]
            self.assertEqual(
                test_unit.run_typescript_tests("/repo", "/artifacts", {}, "npm", "npx"), 1)
            self.assertEqual(run.call_count, 4)

    def test_returns_install_failure_without_running_later_steps(self):
        with (
            mock.patch.object(test_unit.os.path, "isdir", return_value=True),
            mock.patch.object(test_unit.subprocess, "run") as run,
        ):
            run.return_value.returncode = 9
            self.assertEqual(
                test_unit.run_typescript_tests("/repo", "/artifacts", {}, "npm", "npx"), 9)
            run.assert_called_once_with(
                ["npm", "install", "--prefer-offline"], cwd=mock.ANY, env={})


class RunStressReportTestsTest(unittest.TestCase):
    def test_runs_node_tests_with_junit_report_and_propagates_failure(self):
        with mock.patch.object(test_unit.subprocess, "run") as run:
            run.return_value.returncode = 7
            self.assertEqual(
                test_unit.run_stress_report_tests("/repo", "/artifacts", {}, "/tools/node"), 7)

        run.assert_called_once_with([
            "/tools/node", "--test",
            "--test-reporter=spec", "--test-reporter-destination=stdout",
            "--test-reporter=junit",
            "--test-reporter-destination=/artifacts/junit_unit-report.xml",
            "test/stress/generate-report/*_test.js",
        ], cwd="/repo", env=mock.ANY)


class MainTest(unittest.TestCase):
    def test_runs_every_suite_and_returns_first_failure(self):
        runners = [
            "run_go_tests", "run_python_tests", "run_typescript_tests", "run_stress_report_tests"]
        for rcs, want in [
            ((0, 0, 0, 0), 0),
            ((0, 0, 0, 7), 7),
            ((0, 0, 3, 7), 3),
            ((0, 5, 3, 7), 5),
            ((2, 5, 3, 7), 2),
        ]:
            with self.subTest(rcs=rcs), contextlib.ExitStack() as stack:
                stack.enter_context(mock.patch.object(
                    test_unit.utils, "get_repo_root", return_value="/repo"))
                stack.enter_context(mock.patch.object(test_unit.os, "makedirs"))
                ensure_node = stack.enter_context(mock.patch.object(
                    test_unit, "ensure_node", return_value=("/tools/npm", "/tools/npx")))
                stack.enter_context(mock.patch.object(
                    test_unit.shutil, "which", return_value="/tools/node"))
                mocks = [
                    stack.enter_context(mock.patch.object(test_unit, name, return_value=rc))
                    for name, rc in zip(runners, rcs)
                ]
                self.assertEqual(test_unit.main(), want)
                ensure_node.assert_called_once()
                for runner in mocks:
                    runner.assert_called_once()

    def test_returns_failure_and_skips_javascript_suites_when_node_setup_fails(self):
        with (
            mock.patch.object(test_unit.utils, "get_repo_root", return_value="/repo"),
            mock.patch.object(test_unit.os, "makedirs"),
            mock.patch.object(test_unit, "run_go_tests", return_value=0),
            mock.patch.object(test_unit, "run_python_tests", return_value=0),
            mock.patch.object(test_unit, "ensure_node", return_value=(None, None)),
            mock.patch.object(test_unit, "run_typescript_tests") as ts,
            mock.patch.object(test_unit, "run_stress_report_tests") as report,
        ):
            self.assertEqual(test_unit.main(), 1)
        ts.assert_not_called()
        report.assert_not_called()

    def test_passes_node_environment_to_both_javascript_suites(self):
        def ensure_node(repo_root, env):
            env["PATH"] = "/tools/bin"
            return "/tools/bin/npm", "/tools/bin/npx"

        with (
            mock.patch.object(test_unit.utils, "get_repo_root", return_value="/repo"),
            mock.patch.object(test_unit.os, "makedirs"),
            mock.patch.object(test_unit, "run_go_tests", return_value=0),
            mock.patch.object(test_unit, "run_python_tests", return_value=0),
            mock.patch.object(test_unit, "ensure_node", side_effect=ensure_node),
            mock.patch.object(
                test_unit.shutil, "which", return_value="/tools/bin/node") as which,
            mock.patch.object(test_unit, "run_typescript_tests", return_value=0) as ts,
            mock.patch.object(test_unit, "run_stress_report_tests", return_value=0) as report,
        ):
            self.assertEqual(test_unit.main(), 0)

        env = ts.call_args.args[2]
        self.assertEqual(env["PATH"], "/tools/bin")
        which.assert_called_once_with("node", path="/tools/bin")
        ts.assert_called_once_with(
            mock.ANY, mock.ANY, env, "/tools/bin/npm", "/tools/bin/npx")
        report.assert_called_once_with(mock.ANY, mock.ANY, env, "/tools/bin/node")
        self.assertIs(report.call_args.args[2], env)


if __name__ == "__main__":
    unittest.main()
