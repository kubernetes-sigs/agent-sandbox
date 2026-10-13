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

"""Tests for capacity-cliff convergence, including stale rollout status."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

_PATH = Path(__file__).resolve().parents[1] / "load-test/test-recipes/wait_for_warmpool.py"
_SPEC = importlib.util.spec_from_file_location("wait_for_warmpool", _PATH)
waiter = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(waiter)


def pool(uid="pool-1"):
    return {"metadata": {"name": uid, "uid": uid, "namespace": "test", "generation": 2},
            "spec": {"replicas": 1},
            "status": {"observedGeneration": 2, "replicas": 1, "readyReplicas": 1}}


def sandbox(owner="pool-1"):
    return {"metadata": {"name": owner + "-member", "generation": 1,
                         "ownerReferences": [{"uid": owner, "kind": "SandboxWarmPool", "controller": True}]},
            "spec": {"podTemplate": {"spec": {"containers": [
                {"name": "sandbox", "env": [{"name": waiter.REVISION, "value": "2"}]}
            ]}}},
            "status": {"conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}]}}


class ConvergenceTest(unittest.TestCase):
    def test_ready_current_members(self):
        self.assertEqual(waiter.convergence_errors([pool()], [sandbox()], 1, 1, "2"), [])

    def test_template_change_with_unchanged_pool_generation(self):
        member = sandbox()
        member["spec"]["podTemplate"]["spec"]["containers"][0]["env"][0]["value"] = "1"
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_stale_or_missing_pool_status(self):
        for status in ({}, {"observedGeneration": 1, "replicas": 1, "readyReplicas": 1},
                       {"observedGeneration": 2, "replicas": 1, "readyReplicas": 0}):
            with self.subTest(status=status):
                item = pool()
                item["status"] = status
                self.assertTrue(waiter.convergence_errors([item], [sandbox()], 1, 1, "2"))

    def test_rejects_terminating_pool_or_inconsistent_replicas(self):
        for section, key, value in (("metadata", "deletionTimestamp", "2026-10-13T00:00:00Z"),
                                    ("spec", "replicas", 2), ("status", "replicas", 2)):
            with self.subTest(section=section, key=key):
                item = pool()
                item[section][key] = value
                self.assertTrue(waiter.convergence_errors([item], [sandbox()], 1, 1, "2"))

    def test_member_requires_pool_controller_ownership(self):
        for key, value in (("controller", False), ("kind", "SandboxClaim")):
            with self.subTest(key=key):
                member = sandbox()
                member["metadata"]["ownerReferences"][0][key] = value
                self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_member_requires_true_ready_condition(self):
        for conditions in ([], [{"type": "Progressing", "status": "True", "observedGeneration": 1}],
                           [{"type": "Ready", "status": "False", "observedGeneration": 1}],
                           [{"type": "Ready", "status": "Unknown", "observedGeneration": 1}]):
            with self.subTest(conditions=conditions):
                member = sandbox()
                member["status"]["conditions"] = conditions
                self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_rejects_stale_ready_generation(self):
        member = sandbox()
        member["metadata"]["generation"] = 2
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_rejects_terminating_or_extra_members(self):
        member = sandbox()
        member["metadata"]["deletionTimestamp"] = "2026-10-12T00:00:00Z"
        for members in ([member], [sandbox(), member]):
            with self.subTest(count=len(members)):
                self.assertTrue(waiter.convergence_errors([pool()], members, 1, 1, "2"))

    def test_each_pool_must_have_its_own_members(self):
        self.assertTrue(waiter.convergence_errors([pool(), pool("pool-2")],
                                                 [sandbox(), copy.deepcopy(sandbox())], 2, 1, "2"))
        self.assertEqual(waiter.convergence_errors([pool(), pool("pool-2")],
                                                  [sandbox(), sandbox("pool-2")], 2, 1, "2"), [])

    def test_rejects_missing_pool(self):
        self.assertTrue(waiter.convergence_errors([], [], 1, 1, "2"))

    def test_ignores_unrelated_sandbox(self):
        self.assertEqual(waiter.convergence_errors([pool()], [sandbox(), sandbox("other")], 1, 1, "2"), [])

    def test_missing_revision_is_not_converged(self):
        member = sandbox()
        member["spec"]["podTemplate"]["spec"]["containers"][0].pop("env")
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_revision_must_come_from_the_named_container_and_variable(self):
        for unrelated_container in (False, True):
            with self.subTest(unrelated_container=unrelated_container):
                member = sandbox()
                containers = member["spec"]["podTemplate"]["spec"]["containers"]
                containers[0]["env"][0]["value"] = "1"
                if unrelated_container:
                    containers.insert(0, {"name": "sidecar", "env": [{"name": waiter.REVISION, "value": "2"}]})
                else:
                    containers[0]["env"].insert(0, {"name": "UNRELATED", "value": "2"})
                self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_success_survives_unavailable_optional_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {"KUBECONFIG": "/unused", "WARMPOOL_RUN_ID": "test", "WARMPOOL_REPORT_DIR": directory}
            argv = ["wait", "--pools", "1", "--replicas", "1", "--revision", "2", "--stage", "step-1"]
            responses = [json.dumps({"items": [pool()]}), json.dumps({"items": [sandbox()]})]
            responses += [subprocess.CalledProcessError(1, "kubectl", stderr="Forbidden")] * 3
            with patch.dict(waiter.os.environ, env), patch("sys.argv", argv), \
                 patch.object(waiter, "kubectl", side_effect=responses):
                self.assertEqual(waiter.main(), 0)
            output = Path(directory) / "step-1"
            self.assertEqual(json.loads((output / "result.json").read_text())["status"], "converged")
            self.assertEqual(json.loads((output / "sandboxes.json").read_text()), [sandbox()])
            self.assertIn("Forbidden", (output / "controller-metrics.txt.error").read_text())

    def test_timeout_writes_diagnostics_and_returns_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {"KUBECONFIG": "/unused", "WARMPOOL_RUN_ID": "test", "WARMPOOL_REPORT_DIR": directory}
            argv = ["wait", "--pools", "1", "--replicas", "1", "--revision", "2", "--stage", "step-1", "--timeout", "1"]
            with patch.dict(waiter.os.environ, env), patch("sys.argv", argv), \
                 patch.object(waiter, "kubectl", return_value='{"items": []}'), \
                 patch.object(waiter.time, "monotonic", side_effect=[0, 2, 2]):
                self.assertEqual(waiter.main(), 1)
            result = json.loads((Path(directory) / "step-1/result.json").read_text())
            self.assertEqual(result["status"], "timeout")
            self.assertTrue((Path(directory) / "step-1/pools.json").exists())

    def test_api_error_is_not_reported_as_capacity_cliff(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {"KUBECONFIG": "/unused", "WARMPOOL_RUN_ID": "test", "WARMPOOL_REPORT_DIR": directory}
            argv = ["wait", "--pools", "1", "--replicas", "1", "--revision", "2", "--stage", "step-1"]
            with patch.dict(waiter.os.environ, env), patch("sys.argv", argv), \
                 patch.object(waiter, "kubectl", side_effect=subprocess.CalledProcessError(1, "kubectl", stderr="Forbidden")):
                self.assertEqual(waiter.main(), 1)
            result = json.loads((Path(directory) / "step-1/result.json").read_text())
            self.assertEqual(result["status"], "error")
            self.assertIn("Forbidden", result["errors"][0])

    def test_does_not_poll_again_at_exact_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {"KUBECONFIG": "/unused", "WARMPOOL_RUN_ID": "test", "WARMPOOL_REPORT_DIR": directory}
            argv = ["wait", "--pools", "1", "--replicas", "1", "--revision", "2", "--stage", "step-1", "--timeout", "1"]
            with patch.dict(waiter.os.environ, env), patch("sys.argv", argv), \
                 patch.object(waiter, "kubectl", return_value='{"items": []}'), \
                 patch.object(waiter.time, "monotonic", side_effect=[0, 1, 2, 2]), \
                 patch.object(waiter.time, "sleep") as sleep:
                self.assertEqual(waiter.main(), 1)
                sleep.assert_not_called()
            self.assertEqual(json.loads((Path(directory) / "step-1/result.json").read_text())["status"], "timeout")


@unittest.skipUnless(shutil.which("jq"), "runner requires jq")
class RunnerTest(unittest.TestCase):
    def test_recipe_selection_and_failure_propagation(self):
        for workload, exit_code in (("sandbox", 0), ("warmpool", 0), ("warmpool", 7)):
            with self.subTest(workload=workload, exit_code=exit_code), tempfile.TemporaryDirectory() as directory:
                root = Path(directory).resolve()
                tools = root / "tools"
                tools.mkdir()
                (root / "kubeconfig").touch()
                for name, body in {
                    "kubectl": 'printf "%s\\n" "$KUBECONFIG" > "$CAPTURE/preflight"',
                    "go": 'printf "%s\\n" "$KUBECONFIG" "$WARMPOOL_RUN_ID" "$@" > "$CAPTURE/cl2"\nexit "$CL2_EXIT_CODE"',
                }.items():
                    path = tools / name
                    path.write_text("#!/bin/bash\n" + body + "\n")
                    path.chmod(0o755)
                env = {"PATH": str(tools) + os.pathsep + os.environ["PATH"], "HOME": directory,
                       "CAPTURE": directory, "AGENTS_DIR": directory, "CL2_DIR": str(tools),
                       "WORKLOAD": workload, "KUBECONFIG": "kubeconfig", "WARMPOOL_SCENARIO": "many-pools",
                       "STEP_SIZE": "2", "TOTAL_STEPS": "3", "POOL_SIZE": "4",
                       "PROVIDER": "kind", "CL2_EXIT_CODE": str(exit_code)}
                result = subprocess.run(["bash", str(_PATH.with_name("run_capacity_cliff.sh"))],
                                        cwd=root, env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, exit_code, result.stdout + result.stderr)
                args = (root / "cl2").read_text().splitlines()
                self.assertIn(f"--testconfig={directory}/dev/load-test/test-recipes/{workload}-capacity-cliff-test.yaml", args)
                if workload == "warmpool":
                    self.assertEqual(args[0], str(root / "kubeconfig"))
                    self.assertEqual((root / "preflight").read_text().strip(), args[0])
                    override = next(root.glob("dev/load-test/test-recipes/tmp/*/testoverrides.json"))
                    params = json.loads(override.read_text())
                    self.assertEqual(params["CL2_WARMPOOL_RUN_ID"], args[1])
                    self.assertEqual(params["CL2_POOL_SIZE"], 4)
                    self.assertEqual(params["CL2_WARMPOOL_SCENARIO"], "many-pools")


if __name__ == "__main__":
    unittest.main()
