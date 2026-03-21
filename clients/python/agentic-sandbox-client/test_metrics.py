import pytest
from unittest.mock import MagicMock, patch
import time

from k8s_agent_sandbox.sandbox_client import SandboxClient
from k8s_agent_sandbox.metrics import DISCOVERY_LATENCY_MS

@pytest.fixture
def mock_k8s_config():
    with patch('k8s_agent_sandbox.sandbox_client.config.load_incluster_config'), \
         patch('k8s_agent_sandbox.sandbox_client.config.load_kube_config'):
        yield

@pytest.fixture
def mock_custom_objects_api():
    with patch('k8s_agent_sandbox.sandbox_client.client.CustomObjectsApi') as mock_api:
        yield mock_api

@pytest.fixture
def mock_create_claim():
    with patch.object(SandboxClient, '_create_claim') as mock:
        yield mock

@pytest.fixture
def mock_wait_ready():
    with patch.object(SandboxClient, '_wait_for_sandbox_ready') as mock:
        yield mock

@pytest.mark.parametrize(
    "test_name, setup_kwargs, expected_url, should_fail, expected_mode",
    [
        (
            "dev_mode_success",
            {"template_name": "test-template"},
            "http://127.0.0.1:12345",
            False,
            "port_forward"
        ),
        (
            "dev_mode_failure",
            {"template_name": "test-template"},
            None,
            True,
            "port_forward"
        ),
        (
            "gateway_mode_success",
            {"template_name": "test-template", "gateway_name": "test-gw"},
            "http://10.0.0.1",
            False,
            "gateway"
        ),
        (
            "base_url_mode_no_metric",
            {"template_name": "test-template", "api_url": "http://custom-url"},
            "http://custom-url",
            False,
            "preconfigured"
        )
    ]
)
def test_discovery_latency_modes(
    test_name, setup_kwargs, expected_url, should_fail,
    expected_mode,
    mock_k8s_config, mock_custom_objects_api, mock_create_claim, mock_wait_ready
):
    with patch('k8s_agent_sandbox.sandbox_client.subprocess.Popen') as mock_popen, \
         patch('k8s_agent_sandbox.sandbox_client.socket.socket') as mock_socket, \
         patch('k8s_agent_sandbox.sandbox_client.socket.create_connection'), \
         patch('k8s_agent_sandbox.sandbox_client.time.sleep'), \
         patch('k8s_agent_sandbox.sandbox_client.watch.Watch') as mock_watch:

        # Setup mocks based on the test case
        if "dev_mode" in test_name:
            mock_process = MagicMock()
            if should_fail:
                mock_process.poll.return_value = 1
                mock_process.communicate.return_value = (b"", b"Crash")
            else:
                mock_process.poll.return_value = None
            mock_popen.return_value = mock_process

            mock_sock_instance = MagicMock()
            mock_sock_instance.getsockname.return_value = ('0.0.0.0', 12345)
            mock_socket.return_value.__enter__.return_value = mock_sock_instance

        elif "gateway_mode" in test_name:
            mock_w_instance = MagicMock()
            mock_w_instance.stream.return_value = [{
                "type": "ADDED",
                "object": {
                    "status": {
                        "addresses": [{"value": "10.0.0.1"}]
                    }
                }
            }]
            mock_watch.return_value = mock_w_instance

        # Capture metrics before
        try:
            before_success = DISCOVERY_LATENCY_MS.labels(status="success", mode=expected_mode)._sum.get()
        except:
            before_success = 0.0

        try:
            before_failure = DISCOVERY_LATENCY_MS.labels(status="failure", mode=expected_mode)._sum.get()
        except:
            before_failure = 0.0

        client = SandboxClient(**setup_kwargs)

        if should_fail:
            with pytest.raises(RuntimeError):
                with client:
                    pass
        else:
            with client:
                assert client.base_url == expected_url

        # Capture metrics after
        try:
            after_success = DISCOVERY_LATENCY_MS.labels(status="success", mode=expected_mode)._sum.get()
        except:
            after_success = 0.0

        try:
            after_failure = DISCOVERY_LATENCY_MS.labels(status="failure", mode=expected_mode)._sum.get()
        except:
            after_failure = 0.0

        # For preconfigured URLs, we never record a metric.
        if expected_mode == "preconfigured":
            assert after_success == before_success
            assert after_failure == before_failure
        else:
            # For actual discovery modes, assert metric changes
            if should_fail:
                assert after_failure > before_failure
                assert after_success == before_success
            else:
                assert after_success > before_success
                assert after_failure == before_failure
