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

import unittest
from datetime import datetime, timezone
from unittest.mock import patch

from k8s_agent_sandbox.utils import construct_sandbox_claim_lifecycle_spec


class TestConstructSandboxClaimLifecycleSpec(unittest.TestCase):

    @patch("k8s_agent_sandbox.utils.datetime")
    def test_builds_correct_lifecycle_dict(self, mock_datetime):
        frozen_now = datetime(2026, 6, 15, 12, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now

        result = construct_sandbox_claim_lifecycle_spec(300)

        self.assertEqual(result["shutdownTime"], "2026-06-15T12:05:00Z")
        self.assertEqual(result["shutdownPolicy"], "Delete")

    @patch("k8s_agent_sandbox.utils.datetime")
    def test_large_ttl(self, mock_datetime):
        frozen_now = datetime(2026, 1, 1, 0, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now

        result = construct_sandbox_claim_lifecycle_spec(86400)

        self.assertEqual(result["shutdownTime"], "2026-01-02T00:00:00Z")
        self.assertEqual(result["shutdownPolicy"], "Delete")

    def test_rejects_zero(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec(0)
        self.assertIn("positive", str(ctx.exception))

    def test_rejects_negative(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec(-10)
        self.assertIn("positive", str(ctx.exception))

    def test_rejects_bool(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec(True)
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_float(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec(1.5)
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_string(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec("10")
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_overflow(self):
        with self.assertRaises(ValueError) as ctx:
            construct_sandbox_claim_lifecycle_spec(10**18)
        self.assertIn("too large", str(ctx.exception))


class TestOptionalClaimLifecycle(unittest.TestCase):
    @patch("k8s_agent_sandbox.utils.datetime")
    def test_lifecycle_options(self, mock_datetime):
        mock_datetime.now.return_value = datetime(2026, 6, 15, 12, 0, 0, tzinfo=timezone.utc)
        cases = [
            ({}, {}),
            ({"ttl_seconds_after_finished": 0}, {"shutdownPolicy": "Delete", "ttlSecondsAfterFinished": 0}),
            ({"ttl_seconds_after_finished": 2147483647}, {"shutdownPolicy": "Delete", "ttlSecondsAfterFinished": 2147483647}),
            ({"shutdown_policy": "Retain"}, {"shutdownPolicy": "Retain"}),
            ({"shutdown_policy": "DeleteForeground", "shutdown_after_seconds": 300},
             {"shutdownPolicy": "DeleteForeground", "shutdownTime": "2026-06-15T12:05:00Z"}),
        ]
        for arguments, expected in cases:
            with self.subTest(arguments=arguments):
                self.assertEqual(construct_sandbox_claim_lifecycle_spec(**arguments), expected)

    def test_invalid_shutdown_policy(self):
        for policy in ("delete", "Suspend"):
            with self.subTest(policy=policy), self.assertRaisesRegex(ValueError, "shutdown_policy"):
                construct_sandbox_claim_lifecycle_spec(shutdown_policy=policy)

    def test_invalid_ttl_seconds_after_finished(self):
        for ttl in (-1, True, 1.5, 2147483648):
            with self.subTest(ttl=ttl), self.assertRaisesRegex(ValueError, "ttl_seconds_after_finished"):
                construct_sandbox_claim_lifecycle_spec(ttl_seconds_after_finished=ttl)


if __name__ == "__main__":
    unittest.main()
