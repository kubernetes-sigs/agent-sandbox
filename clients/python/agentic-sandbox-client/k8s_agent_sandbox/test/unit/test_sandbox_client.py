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
import logging
from unittest.mock import MagicMock, patch

from kubernetes import config as k8s_config
from k8s_agent_sandbox.k8s_helper import K8sHelper

logger = logging.getLogger(__name__)


class TestK8sHelperWatchNoneEvents(unittest.TestCase):
    """Tests that watch streams gracefully handle None events.

    The `watch` api can yield `None` when the underlying
    connection times out/drops/etc. These tests verify that the watch
    loop can handle this gracefully.
    """

    def setUp(self):
        with patch("kubernetes.config.load_incluster_config", side_effect=k8s_config.ConfigException("not in cluster")), \
             patch("kubernetes.config.load_kube_config"):
            self.helper = K8sHelper()
        self.helper.custom_objects_api = MagicMock()

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_skips_none_events(self, mock_watch_cls):
        """None events from the watch stream should be skipped, not crash."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {"name": "test-sandbox"},
                "status": {
                    "conditions": [
                        {"type": "Ready", "status": "True"},
                    ],
                },
            },
        }
        mock_watch.stream.return_value = [None, None, ready_event]
        self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_sandbox_ready_all_none_times_out(self, mock_watch_cls):
        """A stream of only None events should exhaust the watch and raise TimeoutError."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        mock_watch.stream.return_value = [None, None, None]

        with self.assertRaises(TimeoutError):
            self.helper.wait_for_sandbox_ready("test-sandbox", "default", timeout=10)

    @patch("k8s_agent_sandbox.k8s_helper.watch.Watch")
    def test_wait_for_gateway_ip_skips_none_events(self, mock_watch_cls):
        """None events in the gateway watch should be skipped."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        gateway_ready_event = {
            "type": "MODIFIED",
            "object": {
                "status": {
                    "addresses": [{"value": "10.0.0.1"}],
                },
            },
        }
        mock_watch.stream.return_value = [None, gateway_ready_event]

        ip = self.helper.wait_for_gateway_ip("test-gateway", "default", timeout=10)
        self.assertEqual(ip, "10.0.0.1")


if __name__ == "__main__":
    unittest.main()
