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

from k8s_agent_sandbox.lifecycle import build_lifecycle


class TestBuildLifecycle(unittest.TestCase):

    @patch("k8s_agent_sandbox.lifecycle.datetime")
    def test_builds_correct_lifecycle_dict(self, mock_datetime):
        frozen_now = datetime(2026, 6, 15, 12, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now
        mock_datetime.side_effect = lambda *a, **kw: datetime(*a, **kw)

        result = build_lifecycle(300)

        self.assertEqual(result["shutdownTime"], "2026-06-15T12:05:00Z")
        self.assertEqual(result["shutdownPolicy"], "Delete")

    @patch("k8s_agent_sandbox.lifecycle.datetime")
    def test_large_ttl(self, mock_datetime):
        frozen_now = datetime(2026, 1, 1, 0, 0, 0, tzinfo=timezone.utc)
        mock_datetime.now.return_value = frozen_now
        mock_datetime.side_effect = lambda *a, **kw: datetime(*a, **kw)

        result = build_lifecycle(86400)

        self.assertEqual(result["shutdownTime"], "2026-01-02T00:00:00Z")
        self.assertEqual(result["shutdownPolicy"], "Delete")

    def test_rejects_zero(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle(0)
        self.assertIn("positive", str(ctx.exception))

    def test_rejects_negative(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle(-10)
        self.assertIn("positive", str(ctx.exception))

    def test_rejects_bool(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle(True)
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_float(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle(1.5)
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_string(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle("10")
        self.assertIn("integer", str(ctx.exception))

    def test_rejects_overflow(self):
        with self.assertRaises(ValueError) as ctx:
            build_lifecycle(10**18)
        self.assertIn("too large", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
