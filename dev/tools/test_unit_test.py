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

"""Unit tests for Go package selection in the unit test runner."""

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


if __name__ == "__main__":
    unittest.main()
