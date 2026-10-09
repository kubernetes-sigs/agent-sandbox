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

"""Offline regression tests for the verifier's public command-line entry point.

Exported Bash functions intercept external kubectl calls, so these tests never
read kubeconfig or contact a cluster. They do not replace functional CNI tests.
"""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


VERIFY = Path(__file__).with_name("verify.sh")

MOCK_KUBECTL = r"""() {
  local args=" $* "
  local count=0
  local test_dir="$VERIFIER_TEST_DIR"
  case "$args" in
    *' get namespace '*'-o name '*) return 0 ;;
    *' get namespace '*) printf '%s' owned-test-namespace-uid ;;
    *' get crd '*) return 0 ;;
    *' create -f '*) printf '%s' owned-test-namespace-uid ;;
    *' apply -k '*) return 0 ;;
    *' apply -f '*) touch "$test_dir/claimed" ;;
    *' wait '*) return 0 ;;
    *' get endpointslices '*)
      if [[ -f "$test_dir/claimed" ]]; then
        if [[ -f "$test_dir/not-ready" || -f "$test_dir/deleted" ]]; then
          printf 'pod-b\n'
        else
          printf 'pod-a\npod-b\n'
        fi
      fi ;;
    *' get sandboxclaim serving-a '*) printf '%s' claim-a-uid ;;
    *' get sandboxclaim serving-b '*) printf '%s' claim-b-uid ;;
    *' get pods '*'agents.x-k8s.io/claim-uid=claim-a-uid'*) printf '%s' pod-a ;;
    *' get pods '*'agents.x-k8s.io/claim-uid=claim-b-uid'*) printf '%s' pod-b ;;
    *' exec pod-a '*'touch /tmp/not-ready '*) touch "$test_dir/not-ready" ;;
    *' exec pod-a '*'rm /tmp/not-ready '*) rm "$test_dir/not-ready" ;;
    *' get service claimed-sandbox-entry '*) printf '%s' 10.96.0.20 ;;
    *' get pod pod-b '*) printf '%s' 10.244.0.12 ;;
    *' exec denied-client '*) return 28 ;;
    *' exec allowed-client '*)
      if [[ -f "$test_dir/not-ready" || -f "$test_dir/deleted" ||
            "$args" != *'http://claimed-sandbox-entry/hostname '* ]]; then
        printf '%s' pod-b
        return 0
      fi
      case "$VERIFIER_TEST_SCENARIO" in
        unavailable) return 7 ;;
        one-backend) printf '%s' pod-a; return 0 ;;
        unexpected-backend) printf '%s' unclaimed-reserve; return 0 ;;
        converging) ;;
        *) return 93 ;;
      esac
      if [[ -f "$test_dir/requests" ]]; then
        read -r count < "$test_dir/requests"
      fi
      count=$((count + 1))
      printf '%s\n' "$count" > "$test_dir/requests"
      # EndpointSlice is already ready while forwarding first fails, then only reaches A.
      if [[ "$count" -eq 1 ]]; then return 7; fi
      if [[ "$count" -eq 2 ]]; then return 28; fi
      if [[ "$count" -le 40 ]]; then
        printf '%s' pod-a
      elif (( count % 2 == 1 )); then
        printf '%s' pod-b
      else
        printf '%s' pod-a
      fi ;;
    *' delete sandboxclaim serving-a '*) touch "$test_dir/deleted" ;;
    *' delete namespace '*) touch "$test_dir/cleaned" ;;
    *) printf 'Unexpected intercepted kubectl command: %s\n' "$args" >&2; return 93 ;;
  esac
}"""

# Accelerate only the external sleep boundary; the real verifier's deadlines still run.
MOCK_SLEEP = "() { SECONDS=$((SECONDS + $1)); }"


class VerifyTest(unittest.TestCase):
    def test_rejects_invalid_context_arguments_before_any_kubectl_call(self):
        env = os.environ.copy()
        env["BASH_ENV"] = os.devnull
        env["BASH_FUNC_kubectl%%"] = "() { echo unexpected-kubectl-call >&2; return 93; }"
        for args in ([], [""], ["--context=other"], ["offline-test-context", "extra"]):
            with self.subTest(args=args):
                result = subprocess.run(
                    ["bash", str(VERIFY), *args], env=env, capture_output=True,
                    text=True, timeout=10,
                )
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("Usage:", result.stderr)
                self.assertNotIn("unexpected-kubectl-call", result.stderr)

    def run_scenario(self, scenario):
        with tempfile.TemporaryDirectory(prefix="claimed-sandbox-verifier-test-") as directory:
            env = os.environ.copy()
            env.update({
                "BASH_ENV": os.devnull,
                "BASH_FUNC_kubectl%%": MOCK_KUBECTL,
                "BASH_FUNC_sleep%%": MOCK_SLEEP,
                "VERIFIER_TEST_DIR": directory,
                "VERIFIER_TEST_SCENARIO": scenario,
            })
            result = subprocess.run(
                ["bash", str(VERIFY), "offline-test-context"], env=env,
                capture_output=True, text=True, timeout=10,
            )
            self.assertTrue(Path(directory, "cleaned").exists(), result.stderr)
            return result

    def test_waits_for_initial_connections_and_both_claimed_backends(self):
        result = self.run_scenario("converging")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PASS: the shared Service reaches both claimed Pods", result.stdout)

    def test_permanently_unavailable_service_fails_with_a_bounded_wait(self):
        result = self.run_scenario("unavailable")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("did not reach both claimed Pods before timeout", result.stderr)

    def test_service_reaching_only_one_claimed_backend_fails(self):
        result = self.run_scenario("one-backend")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("did not reach both claimed Pods before timeout", result.stderr)

    def test_service_reaching_an_unclaimed_backend_fails(self):
        result = self.run_scenario("unexpected-backend")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("Service reached unexpected backend unclaimed-reserve", result.stderr)


if __name__ == "__main__":
    unittest.main()
