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

"""Unit tests for AgentSandboxWorkspace.

All tests inject a fake SandboxClient — no cluster, no k8s_agent_sandbox
import, no network. The health check is stubbed per test (its real
implementation is exercised separately against a fake urlopen).
"""

import logging
from importlib.metadata import PackageNotFoundError

import httpx
import pytest

from openhands_k8s_agent_sandbox import workspace as workspace_module
from openhands_k8s_agent_sandbox.workspace import AgentSandboxWorkspace


class FakeSandbox:
    def __init__(self, pod_ip="10.12.0.7", claim_name="sandbox-claim-abc123",
                 sandbox_id="sbx-xyz"):
        self._pod_ip = pod_ip
        self.claim_name = claim_name
        self.sandbox_id = sandbox_id
        self.terminated = 0

    def get_pod_ip(self):
        return self._pod_ip

    def terminate(self):
        self.terminated += 1


class FakeClient:
    def __init__(self, sandbox=None, create_error=None):
        self.sandbox = sandbox or FakeSandbox()
        self.create_error = create_error
        self.create_calls = []
        self.closed = 0

    def create_sandbox(self, warmpool, **kwargs):
        self.create_calls.append((warmpool, kwargs))
        if self.create_error is not None:
            raise self.create_error
        return self.sandbox

    def close(self):
        self.closed += 1


@pytest.fixture
def no_health(monkeypatch):
    """Stub the health wait; provisioning tests don't exercise HTTP."""
    monkeypatch.setattr(
        AgentSandboxWorkspace, "_wait_for_health", lambda self, *, timeout: None
    )


def make_workspace(client=None, **kwargs):
    kwargs.setdefault("warmpool", "agent-server-pool")
    # Offline tests: the advisory version check would issue a real HTTP call.
    kwargs.setdefault("check_server_version", False)
    return AgentSandboxWorkspace(sandbox_client=client or FakeClient(), **kwargs)


# -------------------------------------------------------------- provisioning


def test_provision_sets_host_from_pod_ip(no_health):
    client = FakeClient(FakeSandbox(pod_ip="10.9.8.7"))
    ws = make_workspace(client)
    assert ws.host == "http://10.9.8.7:8000"


def test_explicit_host_is_replaced_by_claimed_endpoint(no_health, caplog):
    # A caller-supplied host must never win over the claimed pod: commands
    # (and the pool session key) would go to an unrelated server while the
    # claim sits idle.
    client = FakeClient(FakeSandbox(pod_ip="10.9.8.7"))
    with caplog.at_level(logging.WARNING):
        ws = make_workspace(client, host="http://elsewhere:9999")
    assert ws.host == "http://10.9.8.7:8000"
    assert any(
        "host=" in r.message and "ignored" in r.message for r in caplog.records
    )
    assert ws.working_dir == "/workspace"


def test_claim_kwargs_forwarded(no_health):
    client = FakeClient()
    make_workspace(
        client,
        namespace="openhands",
        claim_timeout_s=25,
        ttl_s=3600,
        claim_labels={"run": "demo"},
    )
    warmpool, kwargs = client.create_calls[0]
    assert warmpool == "agent-server-pool"
    assert kwargs["namespace"] == "openhands"
    assert kwargs["sandbox_ready_timeout"] == 25
    assert kwargs["shutdown_after_seconds"] == 3600
    assert kwargs["labels"] == {"run": "demo"}


def test_endpoint_template_override(no_health):
    client = FakeClient(FakeSandbox(claim_name="claim-1"))
    ws = make_workspace(
        client,
        namespace="rl",
        endpoint_template="https://gw.example.com/{namespace}/{claim_name}",
    )
    assert ws.host == "https://gw.example.com/rl/claim-1"


def test_custom_server_port(no_health):
    ws = make_workspace(FakeClient(FakeSandbox(pod_ip="10.0.0.2")), server_port=9000)
    assert ws.host == "http://10.0.0.2:9000"


def test_api_key_passthrough(no_health):
    ws = make_workspace(api_key="pool-secret")
    assert ws.api_key == "pool-secret"
    assert ws._headers == {"X-Session-API-Key": "pool-secret"}


def test_ttl_must_be_positive(no_health):
    with pytest.raises(ValueError):
        make_workspace(ttl_s=0)


# -------------------------------------------------------------- error paths


def test_create_failure_propagates():
    client = FakeClient(create_error=RuntimeError("no capacity"))
    with pytest.raises(RuntimeError, match="no capacity"):
        make_workspace(client)


def test_missing_pod_ip_terminates_claim_and_raises(no_health):
    sandbox = FakeSandbox(pod_ip=None)
    client = FakeClient(sandbox)
    with pytest.raises(RuntimeError, match="no pod IP"):
        make_workspace(client)
    assert sandbox.terminated == 1


def test_health_failure_terminates_claim_and_raises(monkeypatch):
    def failing_health(self, *, timeout):
        raise RuntimeError("failed to become healthy")

    monkeypatch.setattr(AgentSandboxWorkspace, "_wait_for_health", failing_health)
    sandbox = FakeSandbox()
    client = FakeClient(sandbox)
    with pytest.raises(RuntimeError, match="healthy"):
        make_workspace(client)
    assert sandbox.terminated == 1


def test_health_check_polls_urlopen(monkeypatch):
    # Real _wait_for_health against a fake urlopen, exercised through
    # construction. First probe refuses, second succeeds.
    calls = []

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(request, timeout):
        calls.append(request)
        if len(calls) == 1:
            raise ConnectionError("not yet")
        return FakeResponse()

    monkeypatch.setattr(
        "openhands_k8s_agent_sandbox.workspace.urlopen", fake_urlopen
    )
    ws = make_workspace(FakeClient(FakeSandbox(pod_ip="10.0.0.3")))
    assert ws.host == "http://10.0.0.3:8000"
    assert len(calls) == 2
    assert calls[0].full_url == "http://10.0.0.3:8000/health"


# ------------------------------------------------------------- router mode


def test_router_mode_host_and_headers(no_health):
    sandbox = FakeSandbox(pod_ip="10.4.4.4", sandbox_id="sbx-777")
    ws = make_workspace(
        FakeClient(sandbox),
        namespace="rl",
        router_url="http://sandbox-router.default.svc:8080/",
        router_auth_token="router-secret",
        api_key="pool-secret",
    )
    assert ws.host == "http://sandbox-router.default.svc:8080"
    headers = ws._headers
    assert headers["X-Sandbox-ID"] == "sbx-777"
    assert headers["X-Sandbox-Namespace"] == "rl"
    assert headers["X-Sandbox-Port"] == "8000"
    assert headers["X-Sandbox-Pod-IP"] == "10.4.4.4"
    assert headers["Authorization"] == "Bearer router-secret"
    # Session auth for the agent-server rides along; the router strips only
    # Authorization before forwarding.
    assert headers["X-Session-API-Key"] == "pool-secret"


def test_router_mode_health_check_sends_routing_headers(monkeypatch):
    captured = []

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def fake_urlopen(request, timeout):
        captured.append(request)
        return FakeResponse()

    monkeypatch.setattr(
        "openhands_k8s_agent_sandbox.workspace.urlopen", fake_urlopen
    )
    make_workspace(
        FakeClient(FakeSandbox(sandbox_id="sbx-9")),
        router_url="http://router:8080",
        router_auth_token="tok",
    )
    request = captured[0]
    assert request.full_url == "http://router:8080/health"
    # urllib capitalizes header names on add
    assert request.get_header("X-sandbox-id") == "sbx-9"
    assert request.get_header("Authorization") == "Bearer tok"


def test_router_and_endpoint_template_are_exclusive():
    client = FakeClient()
    with pytest.raises(ValueError, match="mutually exclusive"):
        make_workspace(
            client,
            router_url="http://router:8080",
            endpoint_template="http://gw/{claim_name}",
        )
    assert client.create_calls == []  # rejected before any claim


# -------------------------------------------------------- version skew check


def test_version_skew_warns(no_health, monkeypatch, caplog):
    monkeypatch.setattr(
        AgentSandboxWorkspace, "get_server_info",
        lambda self: {"version": "0.0.1-other"},
    )
    with caplog.at_level(logging.WARNING):
        make_workspace(check_server_version=True)
    assert any("protocol skew" in r.message for r in caplog.records)


@pytest.mark.parametrize(
    "error",
    [httpx.ConnectError("unreachable"), ValueError("body is not JSON")],
    ids=["transport", "not-json"],
)
def test_version_check_expected_failures_are_silent(
    no_health, monkeypatch, caplog, error
):
    def boom(self):
        raise error

    monkeypatch.setattr(AgentSandboxWorkspace, "get_server_info", boom)
    with caplog.at_level(logging.WARNING):
        make_workspace(check_server_version=True)
    assert not [r for r in caplog.records if r.levelno >= logging.WARNING]


def test_version_check_missing_sdk_dist_is_silent(no_health, monkeypatch, caplog):
    monkeypatch.setattr(
        AgentSandboxWorkspace, "get_server_info", lambda self: {"version": "1.0"}
    )

    def missing(_name):
        raise PackageNotFoundError("openhands-sdk")

    monkeypatch.setattr(workspace_module, "_dist_version", missing)
    with caplog.at_level(logging.WARNING):
        make_workspace(check_server_version=True)
    assert not [r for r in caplog.records if r.levelno >= logging.WARNING]


def test_version_check_unexpected_payload_warns(no_health, monkeypatch, caplog):
    # A non-dict payload must not raise (AttributeError on .get) nor vanish.
    monkeypatch.setattr(
        AgentSandboxWorkspace, "get_server_info", lambda self: ["1.0"]
    )
    with caplog.at_level(logging.WARNING):
        ws = make_workspace(check_server_version=True)
    assert ws.host.startswith("http://")  # attach still succeeded
    warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
    assert len(warnings) == 1
    assert "expected a JSON object" in warnings[0].message


def test_version_check_bug_is_loud_but_not_fatal(no_health, monkeypatch, caplog):
    def bug(self):
        raise TypeError("bug in the check")

    monkeypatch.setattr(AgentSandboxWorkspace, "get_server_info", bug)
    with caplog.at_level(logging.WARNING):
        ws = make_workspace(check_server_version=True)
    assert ws.host.startswith("http://")
    warnings = [r for r in caplog.records if r.levelno == logging.WARNING]
    assert len(warnings) == 1
    assert "failed unexpectedly" in warnings[0].message
    assert warnings[0].exc_info is not None  # traceback attached, not hidden


# ----------------------------------------------------------------- teardown


def test_cleanup_terminates_once(no_health):
    sandbox = FakeSandbox()
    ws = make_workspace(FakeClient(sandbox))
    ws.cleanup()
    ws.cleanup()
    assert sandbox.terminated == 1


def test_context_manager_cleans_up(no_health):
    sandbox = FakeSandbox()
    with make_workspace(FakeClient(sandbox)) as ws:
        assert ws.host.startswith("http://")
    assert sandbox.terminated == 1


def test_cleanup_logs_release_for_owning_sandbox(no_health, caplog):
    ws = make_workspace(FakeClient(FakeSandbox(claim_name="claim-own")))
    with caplog.at_level(logging.INFO):
        ws.cleanup()
    messages = [r.message for r in caplog.records]
    assert "Released sandbox claim claim-own" in messages


def test_cleanup_logs_detach_for_non_releasing_view(no_health, caplog):
    # Injected views (e.g. a fleet handle bound for fleet.run) whose
    # terminate() is a no-op on the pod must not claim a release.
    sandbox = FakeSandbox(claim_name="claim-bound")
    sandbox.releases_on_terminate = False
    ws = make_workspace(FakeClient(sandbox))
    with caplog.at_level(logging.INFO):
        ws.cleanup()
    messages = [r.message for r in caplog.records]
    assert sandbox.terminated == 1
    assert not any("Released sandbox claim" in m for m in messages)
    assert any("Detached from sandbox claim-bound" in m for m in messages)


def test_injected_client_not_closed(no_health):
    client = FakeClient()
    ws = make_workspace(client)
    ws.cleanup()
    # We only close clients we built ourselves.
    assert client.closed == 0


def test_terminate_error_is_swallowed(no_health):
    sandbox = FakeSandbox()

    def boom():
        sandbox.terminated += 1
        raise RuntimeError("apiserver hiccup")

    sandbox.terminate = boom
    ws = make_workspace(FakeClient(sandbox))
    ws.cleanup()  # must not raise; ttl_s is the backstop
    assert sandbox.terminated == 1


def test_cleanup_retries_after_failed_terminate(no_health):
    # First attempt fails -> the claim reference is kept and the next
    # cleanup() retries; only success clears it (else a no-ttl claim leaks).
    sandbox = FakeSandbox()
    attempts = []

    def flaky():
        attempts.append(True)
        if len(attempts) == 1:
            raise RuntimeError("apiserver hiccup")
        sandbox.terminated += 1

    sandbox.terminate = flaky
    ws = make_workspace(FakeClient(sandbox))
    ws.cleanup()
    assert ws._sandbox is not None  # kept for retry
    ws.cleanup()
    assert sandbox.terminated == 1
    assert ws._sandbox is None
    ws.cleanup()  # idempotent after success
    assert len(attempts) == 2
