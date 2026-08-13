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

import importlib.util
import os
import unittest
from datetime import datetime, timezone
from unittest import mock


def _load_create_claim(mock_api, env=None):
    # create-claim.py isn't a valid module name (hyphen) and the README
    # documents running it directly (`python3 create-claim.py`), so it's
    # loaded by file path rather than renamed. Its top level reads
    # NAMESPACE/WARM_POOL_NAME from the environment and calls
    # config.load_kube_config() + CustomObjectsApi(), both of which would
    # try to reach a real cluster, so both are mocked out for the load.
    with mock.patch.dict(os.environ, env or {}, clear=False), \
         mock.patch("kubernetes.config.load_kube_config"), \
         mock.patch("kubernetes.client.CustomObjectsApi", return_value=mock_api):
        spec = importlib.util.spec_from_file_location(
            "keda_scale_to_zero_create_claim",
            os.path.join(os.path.dirname(__file__), "create-claim.py"),
        )
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


class CreateClaimTest(unittest.TestCase):
    def test_sends_expected_sandboxclaim_body(self):
        mock_api = mock.MagicMock()
        module = _load_create_claim(mock_api)

        before = datetime.now(timezone.utc)
        result = module.create_claim(7)
        after = datetime.now(timezone.utc)

        self.assertTrue(result)
        mock_api.create_namespaced_custom_object.assert_called_once()
        kwargs = mock_api.create_namespaced_custom_object.call_args.kwargs
        self.assertEqual(kwargs["group"], "extensions.agents.x-k8s.io")
        self.assertEqual(kwargs["version"], "v1beta1")
        self.assertEqual(kwargs["namespace"], module.NAMESPACE)
        self.assertEqual(kwargs["plural"], "sandboxclaims")

        body = kwargs["body"]
        self.assertEqual(body["kind"], "SandboxClaim")
        self.assertEqual(body["spec"]["warmPoolRef"]["name"], module.WARMPOOL)
        self.assertEqual(body["spec"]["lifecycle"]["shutdownPolicy"], "Delete")
        self.assertTrue(body["metadata"]["name"].startswith("loadtest-"))
        self.assertTrue(body["metadata"]["name"].endswith("-7"))

        # shutdownTime should be ~CLAIM_TTL_SECONDS in the future, in RFC3339 Zulu form.
        shutdown_time = datetime.strptime(
            body["spec"]["lifecycle"]["shutdownTime"], "%Y-%m-%dT%H:%M:%SZ"
        ).replace(tzinfo=timezone.utc)
        expected_min = before.timestamp() + module.CLAIM_TTL_SECONDS - 1
        expected_max = after.timestamp() + module.CLAIM_TTL_SECONDS + 1
        self.assertTrue(expected_min <= shutdown_time.timestamp() <= expected_max)

    def test_returns_false_and_swallows_api_errors(self):
        mock_api = mock.MagicMock()
        mock_api.create_namespaced_custom_object.side_effect = RuntimeError("boom")
        module = _load_create_claim(mock_api)

        result = module.create_claim(0)  # must not raise

        self.assertFalse(result)

    def test_namespace_and_warmpool_read_from_env(self):
        mock_api = mock.MagicMock()
        module = _load_create_claim(
            mock_api, env={"NAMESPACE": "custom-ns", "WARM_POOL_NAME": "custom-pool"}
        )

        self.assertEqual(module.NAMESPACE, "custom-ns")
        self.assertEqual(module.WARMPOOL, "custom-pool")

        module.create_claim(1)
        kwargs = mock_api.create_namespaced_custom_object.call_args.kwargs
        self.assertEqual(kwargs["namespace"], "custom-ns")
        self.assertEqual(kwargs["body"]["spec"]["warmPoolRef"]["name"], "custom-pool")


if __name__ == "__main__":
    unittest.main()
