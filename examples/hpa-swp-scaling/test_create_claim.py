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
from datetime import datetime, timedelta, timezone
from unittest import mock


class _StopLoop(Exception):
    """Sentinel used to break delete_expired_claims()'s `while True` after one pass."""


def _load_create_claim(mock_api):
    # create-claim.py isn't a valid module name (hyphen) and the README
    # documents running it directly (`python create-claim.py`), so it's
    # loaded by file path rather than renamed. Its top level calls
    # config.load_kube_config() and instantiates CustomObjectsApi(), both of
    # which would try to reach a real cluster, so both are mocked out for the
    # duration of the load.
    with mock.patch("kubernetes.config.load_kube_config"), \
         mock.patch("kubernetes.client.CustomObjectsApi", return_value=mock_api):
        spec = importlib.util.spec_from_file_location(
            "hpa_swp_scaling_create_claim",
            os.path.join(os.path.dirname(__file__), "create-claim.py"),
        )
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


class CreateClaimTest(unittest.TestCase):
    def test_sends_expected_sandboxclaim_body(self):
        mock_api = mock.MagicMock()
        module = _load_create_claim(mock_api)

        module.create_claim(3)

        mock_api.create_namespaced_custom_object.assert_called_once()
        kwargs = mock_api.create_namespaced_custom_object.call_args.kwargs
        self.assertEqual(kwargs["group"], "extensions.agents.x-k8s.io")
        self.assertEqual(kwargs["version"], "v1beta1")
        self.assertEqual(kwargs["namespace"], module.NAMESPACE)
        self.assertEqual(kwargs["plural"], "sandboxclaims")

        body = kwargs["body"]
        self.assertEqual(body["kind"], "SandboxClaim")
        self.assertEqual(body["spec"]["warmPoolRef"]["name"], module.WARMPOOL)
        self.assertTrue(body["metadata"]["name"].startswith("loadtest-"))
        self.assertTrue(body["metadata"]["name"].endswith("-3"))

    def test_swallows_api_errors_instead_of_raising(self):
        mock_api = mock.MagicMock()
        mock_api.create_namespaced_custom_object.side_effect = RuntimeError("boom")
        module = _load_create_claim(mock_api)

        module.create_claim(0)  # must not raise


class DeleteExpiredClaimsTest(unittest.TestCase):
    def test_deletes_only_claims_past_ttl(self):
        mock_api = mock.MagicMock()
        module = _load_create_claim(mock_api)

        now = datetime.now(timezone.utc)
        expired_ts = (now - timedelta(seconds=module.CLAIM_TTL_SECONDS + 10)).strftime("%Y-%m-%dT%H:%M:%SZ")
        fresh_ts = (now - timedelta(seconds=5)).strftime("%Y-%m-%dT%H:%M:%SZ")
        mock_api.list_namespaced_custom_object.return_value = {
            "items": [
                {"metadata": {"name": "expired-claim", "creationTimestamp": expired_ts}},
                {"metadata": {"name": "fresh-claim", "creationTimestamp": fresh_ts}},
            ]
        }

        with mock.patch("time.sleep", side_effect=_StopLoop):
            with self.assertRaises(_StopLoop):
                module.delete_expired_claims()

        mock_api.delete_namespaced_custom_object.assert_called_once()
        kwargs = mock_api.delete_namespaced_custom_object.call_args.kwargs
        self.assertEqual(kwargs["name"], "expired-claim")

    def test_no_deletions_when_nothing_expired(self):
        mock_api = mock.MagicMock()
        module = _load_create_claim(mock_api)

        fresh_ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        mock_api.list_namespaced_custom_object.return_value = {
            "items": [{"metadata": {"name": "fresh-claim", "creationTimestamp": fresh_ts}}]
        }

        with mock.patch("time.sleep", side_effect=_StopLoop):
            with self.assertRaises(_StopLoop):
                module.delete_expired_claims()

        mock_api.delete_namespaced_custom_object.assert_not_called()


if __name__ == "__main__":
    unittest.main()
