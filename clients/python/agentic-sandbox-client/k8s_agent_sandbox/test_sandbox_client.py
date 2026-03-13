import unittest
import logging
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.constants import POD_NAME_ANNOTATION

logger = logging.getLogger(__name__)


class TestSandboxClientWatchNoneEvents(unittest.TestCase):
    """Tests that watch streams gracefully handle None events.

    The `watch` api can yield `None` when the underlying
    connection times out/drops/etc. These tests verify that the watch
    loop can handle this gracefully.
    """

    def setUp(self):
        with patch.object(SandboxClient, "__init__", return_value=None):
            self.client = SandboxClient("test-template")

        self.client.custom_objects_api = MagicMock()
        self.client.claim_name = "test-claim"
        self.client.sandbox_name = None
        self.client.pod_name = None
        self.client.annotations = None
        self.client.namespace = "default"
        self.client.sandbox_ready_timeout = 10
        self.client.gateway_ready_timeout = 10
        self.client.gateway_name = "test-gateway"
        self.client.gateway_namespace = "default"
        self.client.base_url = None
        self.client.tracing_manager = None

    @patch("k8s_agent_sandbox.sandbox_client.watch.Watch")
    def test_wait_for_sandbox_ready_skips_none_events(self, mock_watch_cls):
        """None events from the watch stream should be skipped, not crash."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        ready_event = {
            "type": "MODIFIED",
            "object": {
                "metadata": {
                    "name": "test-sandbox",
                    "annotations": {POD_NAME_ANNOTATION: "test-pod"},
                },
                "status": {
                    "conditions": [
                        {"type": "Ready", "status": "True"},
                    ],
                },
            },
        }
        mock_watch.stream.return_value = [None, None, ready_event]
        self.client._wait_for_sandbox_ready()

        self.assertEqual(self.client.sandbox_name, "test-sandbox")
        self.assertEqual(self.client.pod_name, "test-pod")

    @patch("k8s_agent_sandbox.sandbox_client.watch.Watch")
    def test_wait_for_sandbox_ready_all_none_times_out(self, mock_watch_cls):
        """A stream of only None events should exhaust the watch and raise TimeoutError."""
        mock_watch = MagicMock()
        mock_watch_cls.return_value = mock_watch

        mock_watch.stream.return_value = [None, None, None]

        with patch.object(self.client, "__exit__"):
            with self.assertRaises(TimeoutError):
                self.client._wait_for_sandbox_ready()

    @patch("k8s_agent_sandbox.sandbox_client.watch.Watch")
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

        self.client._wait_for_gateway_ip()
        self.assertEqual(self.client.base_url, "http://10.0.0.1")


if __name__ == "__main__":
    unittest.main()
