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

_PATH = Path(__file__).with_name("wait_for_warmpool.py")
_SPEC = importlib.util.spec_from_file_location("wait_for_warmpool", _PATH)
waiter = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(waiter)


def pool(uid="pool-1"):
    """Build a pool whose counters reflect its current generation."""
    return {"metadata": {"name": uid, "uid": uid, "namespace": "test", "generation": 2},
            "spec": {"replicas": 1},
            "status": {"observedGeneration": 2, "replicas": 1, "readyReplicas": 1}}


def sandbox(owner="pool-1"):
    """Build a Ready member at the requested template revision."""
    return {"metadata": {"name": owner + "-member", "generation": 1,
                         "ownerReferences": [{"uid": owner, "kind": "SandboxWarmPool", "controller": True}]},
            "spec": {"podTemplate": {"spec": {"containers": [
                {"name": "sandbox", "env": [{"name": waiter.REVISION, "value": "2"}]}
            ]}}},
            "status": {"conditions": [{"type": "Ready", "status": "True", "observedGeneration": 1}]}}


class ConvergenceTest(unittest.TestCase):
    def test_ready_current_members(self):
        """Accept a complete current-generation observation."""
        self.assertEqual(waiter.convergence_errors([pool()], [sandbox()], 1, 1, "2"), [])

    def test_template_change_with_unchanged_pool_generation(self):
        """A template edit must invalidate old members even if the pool generation is unchanged."""
        member = sandbox()
        member["spec"]["podTemplate"]["spec"]["containers"][0]["env"][0]["value"] = "1"
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_stale_or_missing_pool_status(self):
        """Missing or outdated counters cannot prove convergence."""
        for status in ({}, {"observedGeneration": 1, "replicas": 1, "readyReplicas": 1},
                       {"observedGeneration": 2, "replicas": 1, "readyReplicas": 0}):
            with self.subTest(status=status):
                item = pool()
                item["status"] = status
                self.assertTrue(waiter.convergence_errors([item], [sandbox()], 1, 1, "2"))

    def test_rejects_terminating_pool_or_inconsistent_replicas(self):
        """A matching Ready counter cannot hide deletion or a different desired size."""
        for section, key, value in (("metadata", "deletionTimestamp", "2026-10-13T00:00:00Z"),
                                    ("spec", "replicas", 2), ("status", "replicas", 2)):
            with self.subTest(section=section, key=key):
                item = pool()
                item[section][key] = value
                self.assertTrue(waiter.convergence_errors([item], [sandbox()], 1, 1, "2"))

    def test_member_requires_pool_controller_ownership(self):
        """Non-controller references must not satisfy the member count."""
        for key, value in (("controller", False), ("kind", "SandboxClaim")):
            with self.subTest(key=key):
                member = sandbox()
                member["metadata"]["ownerReferences"][0][key] = value
                self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_member_requires_true_ready_condition(self):
        """Only an affirmative Ready condition proves readiness."""
        for conditions in ([], [{"type": "Progressing", "status": "True", "observedGeneration": 1}],
                           [{"type": "Ready", "status": "False", "observedGeneration": 1}],
                           [{"type": "Ready", "status": "Unknown", "observedGeneration": 1}]):
            with self.subTest(conditions=conditions):
                member = sandbox()
                member["status"]["conditions"] = conditions
                self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_rejects_stale_ready_generation(self):
        """Readiness from a previous member generation is insufficient."""
        member = sandbox()
        member["metadata"]["generation"] = 2
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_rejects_terminating_or_extra_members(self):
        """Old members must finish deletion before a rollout converges."""
        member = sandbox()
        member["metadata"]["deletionTimestamp"] = "2026-10-12T00:00:00Z"
        for members in ([member], [sandbox(), member]):
            with self.subTest(count=len(members)):
                self.assertTrue(waiter.convergence_errors([pool()], members, 1, 1, "2"))

    def test_each_pool_must_have_its_own_members(self):
        """An aggregate member count cannot hide an empty pool."""
        self.assertTrue(waiter.convergence_errors([pool(), pool("pool-2")],
                                                 [sandbox(), copy.deepcopy(sandbox())], 2, 1, "2"))
        self.assertEqual(waiter.convergence_errors([pool(), pool("pool-2")],
                                                  [sandbox(), sandbox("pool-2")], 2, 1, "2"), [])

    def test_rejects_missing_pool(self):
        """An empty LIST cannot satisfy a positive target."""
        self.assertTrue(waiter.convergence_errors([], [], 1, 1, "2"))

    def test_ignores_unrelated_sandbox(self):
        """Other owners must not influence this run."""
        self.assertEqual(waiter.convergence_errors([pool()], [sandbox(), sandbox("other")], 1, 1, "2"), [])

    def test_missing_revision_is_not_converged(self):
        """Members without the revision marker cannot prove a rollout."""
        member = sandbox()
        member["spec"]["podTemplate"]["spec"]["containers"][0].pop("env")
        self.assertTrue(waiter.convergence_errors([pool()], [member], 1, 1, "2"))

    def test_revision_must_come_from_the_named_container_and_variable(self):
        """Unrelated environment values must not hide an old sandbox revision."""
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

class ObservationTest(unittest.TestCase):
    def setUp(self):
        """Keep reports and the polling clock isolated from the host environment."""
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.clock = 0

    def run_wait(self, response, timeout=60):
        """Run one stage with a virtual clock and a controlled API response."""
        env = {"KUBECONFIG": "/unused", "WARMPOOL_RUN_ID": "test", "WARMPOOL_REPORT_DIR": str(self.root)}
        argv = ["wait", "--pools", "1", "--replicas", "1", "--revision", "2", "--stage", "step-1",
                "--timeout", str(timeout), "--interval", "1"]
        def sleep(seconds):
            self.clock += seconds
        with patch.dict(waiter.os.environ, env), patch("sys.argv", argv), \
             patch.object(waiter.time, "monotonic", side_effect=lambda: self.clock), \
             patch.object(waiter.time, "sleep", side_effect=sleep), \
             patch.object(waiter, "kubectl", side_effect=response) as command:
            code = waiter.main()
        result = json.loads((self.root / "step-1/result.json").read_text())
        return code, result, command

    def test_success_survives_unavailable_optional_metrics(self):
        """Optional metrics failures must preserve successful convergence."""
        responses = [json.dumps({"items": [pool()]}), json.dumps({"items": [sandbox()]}),
                     subprocess.CalledProcessError(1, "kubectl", stderr="Forbidden")]
        code, result, command = self.run_wait(responses)
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "converged")
        self.assertFalse((self.root / "step-1/sandboxes.json").exists())
        self.assertFalse((self.root / "first-failure.txt").exists())
        self.assertIn("Forbidden", (self.root / "step-1/controller-metrics.txt.error").read_text())
        self.assertIn("agents.x-k8s.io/warm-pool-sandbox", command.call_args_list[1].args)
        self.assertEqual(command.call_count, 3)

    def test_list_can_take_longer_than_twenty_seconds(self):
        """Slow LISTs share the configured stage deadline without the old fixed cap."""
        def response(*args, timeout):
            if "sandboxwarmpools" in args:
                self.assertEqual(timeout, 60)
                self.clock += 25
                return json.dumps({"items": [pool()]})
            if "sandboxes" in args:
                self.assertEqual(timeout, 35)
                self.clock += 25
                return json.dumps({"items": [sandbox()]})
            return "metrics"
        code, result, _ = self.run_wait(response)
        self.assertEqual(code, 0)
        self.assertEqual(result["elapsed_seconds"], 50)

    def test_does_not_poll_again_at_exact_deadline(self):
        """Stop after the final polling sleep instead of starting another LIST."""
        code, result, command = self.run_wait(lambda *a, **kw: '{"items": []}', timeout=1)
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "timeout")
        self.assertEqual(sum("sandboxwarmpools" in c.args for c in command.call_args_list), 1)
        self.assertTrue((self.root / "step-1/pools.json").exists())

    def test_read_finishing_at_deadline_cannot_report_success(self):
        """Late observations must not turn a missed deadline into success."""
        def response(*args, **kwargs):
            if "sandboxwarmpools" in args:
                return json.dumps({"items": [pool()]})
            if "sandboxes" in args:
                self.clock = 60
                return json.dumps({"items": [sandbox()]})
            return "{}"
        code, result, _ = self.run_wait(response)
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "timeout")

    def test_api_errors_are_not_reported_as_capacity_cliffs(self):
        """Incomplete observations are tooling errors, not capacity measurements."""
        for error in (subprocess.CalledProcessError(1, "kubectl", stderr="Forbidden"),
                      subprocess.TimeoutExpired("kubectl", 60)):
            with self.subTest(error=error):
                code, result, _ = self.run_wait(error)
                self.assertEqual(code, 1)
                self.assertEqual(result["status"], "error")
                self.assertTrue(result["errors"])

    def test_full_snapshots_only_at_first_failure(self):
        """Continuing CL2 stages must not multiply full resource dumps."""
        for stage, status in (("success", "converged"), ("failure", "timeout"), ("later", "timeout")):
            output = self.root / stage
            output.mkdir()
            with patch.object(waiter, "kubectl", return_value="{}") as command:
                waiter.snapshot(output, [pool()], [sandbox()], ["test"], status)
            self.assertEqual(command.call_count, 3 if stage == "failure" else 1)
            self.assertEqual((output / "pools.json").exists(), stage == "failure")
            self.assertEqual((output / "sandboxes.json").exists(), stage == "failure")
        self.assertEqual((self.root / "first-failure.txt").read_text(), "failure\n")
        self.assertEqual(json.loads((self.root / "failure/sandboxes.json").read_text()), [sandbox()])

    def test_diagnostics_share_one_bounded_budget(self):
        """An exhausted diagnostic budget must prevent further subprocesses."""
        output = self.root / "failure"
        output.mkdir()
        def response(*args, timeout, **kwargs):
            self.assertEqual(timeout, 120)
            self.clock = 120
            raise subprocess.TimeoutExpired("kubectl", timeout)
        with patch.object(waiter.time, "monotonic", side_effect=lambda: self.clock), \
             patch.object(waiter.subprocess, "check_output", side_effect=response) as command:
            waiter.snapshot(output, [], [], ["test"], "timeout")
        self.assertEqual(command.call_count, 1)
        self.assertEqual(len(list(output.glob("*.error"))), 3)

    def test_kubectl_uses_remaining_budget_for_request_and_process(self):
        """Both timeout mechanisms must honor the caller budget."""
        with patch.object(waiter.subprocess, "check_output", return_value="{}") as command:
            waiter.kubectl("get", "sandboxes", timeout=1750)
        self.assertIn("--request-timeout=1750.000s", command.call_args.args[0])
        self.assertEqual(command.call_args.kwargs["timeout"], 1750)
        with patch.object(waiter.subprocess, "check_output") as command:
            with self.assertRaises(subprocess.TimeoutExpired):
                waiter.kubectl("get", "sandboxes", timeout=0)
            command.assert_not_called()


@unittest.skipUnless(shutil.which("jq"), "runner requires jq")
class RunnerTest(unittest.TestCase):
    def test_recipe_selection_and_failure_propagation(self):
        """The wrapper must select the intended recipe and preserve CL2 failures."""
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
