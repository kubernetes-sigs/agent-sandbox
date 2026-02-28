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

import socket
from unittest.mock import MagicMock

import pytest

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox import sandbox_client as sandbox_client_module


def _build_client(
    monkeypatch: pytest.MonkeyPatch,
    custom_objects_api: MagicMock,
    template_name: str = "python-template",
    claim_name: str | None = None,
    api_url: str | None = None,
) -> SandboxClient:
    def _raise_incluster() -> None:
        raise sandbox_client_module.config.ConfigException("not running in cluster")

    monkeypatch.setattr(
        sandbox_client_module.config, "load_incluster_config", _raise_incluster
    )
    monkeypatch.setattr(sandbox_client_module.config, "load_kube_config", lambda: None)
    monkeypatch.setattr(
        sandbox_client_module.client, "CustomObjectsApi", lambda: custom_objects_api
    )
    return SandboxClient(
        template_name=template_name,
        claim_name=claim_name,
        api_url=api_url,
        delete_on_exit=False,
    )


def test_setup_claim_reconnect_with_matching_template(monkeypatch: pytest.MonkeyPatch):
    custom_objects_api = MagicMock()
    custom_objects_api.get_namespaced_custom_object.return_value = {
        "spec": {"sandboxTemplateRef": {"name": "python-template"}}
    }
    sandbox_client = _build_client(
        monkeypatch,
        custom_objects_api,
        template_name="python-template",
        claim_name="reused-claim",
    )

    sandbox_client._setup_claim()

    assert sandbox_client.claim_name == "reused-claim"
    assert sandbox_client.was_reconnected is True
    custom_objects_api.create_namespaced_custom_object.assert_not_called()


def test_setup_claim_reconnect_fails_on_template_mismatch(monkeypatch: pytest.MonkeyPatch):
    custom_objects_api = MagicMock()
    custom_objects_api.get_namespaced_custom_object.return_value = {
        "spec": {"sandboxTemplateRef": {"name": "other-template"}}
    }
    sandbox_client = _build_client(
        monkeypatch,
        custom_objects_api,
        template_name="python-template",
        claim_name="reused-claim",
    )

    with pytest.raises(RuntimeError) as exc_info:
        sandbox_client._setup_claim()

    assert "reused-claim" in str(exc_info.value)
    assert "python-template" in str(exc_info.value)
    assert "other-template" in str(exc_info.value)
    assert sandbox_client.was_reconnected is False
    custom_objects_api.create_namespaced_custom_object.assert_not_called()


def test_delete_clears_claim_name(monkeypatch: pytest.MonkeyPatch):
    custom_objects_api = MagicMock()
    sandbox_client = _build_client(monkeypatch, custom_objects_api)
    sandbox_client.claim_name = "delete-me"

    assert sandbox_client.delete() is True
    assert sandbox_client.claim_name is None
    custom_objects_api.delete_namespaced_custom_object.assert_called_once()


def test_enter_waits_for_api_ready(monkeypatch: pytest.MonkeyPatch):
    custom_objects_api = MagicMock()
    sandbox_client = _build_client(
        monkeypatch,
        custom_objects_api,
        api_url="http://sb-abc.sandbox-router",
    )

    setup_claim = MagicMock()
    wait_for_ready = MagicMock()
    wait_for_api = MagicMock()
    wait_for_gateway = MagicMock()
    start_port_forward = MagicMock()

    monkeypatch.setattr(sandbox_client, "_setup_claim", setup_claim)
    monkeypatch.setattr(sandbox_client, "_wait_for_sandbox_ready", wait_for_ready)
    monkeypatch.setattr(sandbox_client, "_wait_for_api_ready", wait_for_api)
    monkeypatch.setattr(sandbox_client, "_wait_for_gateway_ip", wait_for_gateway)
    monkeypatch.setattr(
        sandbox_client, "_start_and_wait_for_port_forward", start_port_forward
    )

    assert sandbox_client.__enter__() is sandbox_client
    setup_claim.assert_called_once()
    wait_for_ready.assert_called_once()
    wait_for_api.assert_called_once()
    wait_for_gateway.assert_not_called()
    start_port_forward.assert_not_called()


def test_wait_for_api_ready_retries_until_resolvable_and_reachable(
    monkeypatch: pytest.MonkeyPatch,
):
    custom_objects_api = MagicMock()
    sandbox_client = _build_client(
        monkeypatch,
        custom_objects_api,
        api_url="http://sb-abc.sandbox-router",
    )
    sandbox_client.api_ready_timeout = 1
    sandbox_client.api_probe_interval = 0

    dns_attempts = {"count": 0}
    http_attempts = {"count": 0}

    def fake_getaddrinfo(host: str, port: int, type: int):
        assert host == "sb-abc.sandbox-router"
        assert port == 80
        assert type == socket.SOCK_STREAM
        dns_attempts["count"] += 1
        if dns_attempts["count"] < 3:
            raise socket.gaierror(-3, "temporary failure in name resolution")
        return [("ok",)]

    def fake_get(url: str, timeout: float, allow_redirects: bool):
        assert url == "http://sb-abc.sandbox-router"
        assert timeout == 1.5
        assert allow_redirects is False
        http_attempts["count"] += 1
        if http_attempts["count"] == 1:
            raise sandbox_client_module.requests.exceptions.ConnectionError(
                "connection refused"
            )
        response = MagicMock()
        response.status_code = 404
        return response

    monkeypatch.setattr(sandbox_client_module.socket, "getaddrinfo", fake_getaddrinfo)
    monkeypatch.setattr(sandbox_client_module.requests, "get", fake_get)
    monkeypatch.setattr(sandbox_client_module.time, "sleep", lambda _: None)

    sandbox_client._wait_for_api_ready()
    assert dns_attempts["count"] == 4
    assert http_attempts["count"] == 2


def test_wait_for_api_ready_times_out(monkeypatch: pytest.MonkeyPatch):
    custom_objects_api = MagicMock()
    sandbox_client = _build_client(
        monkeypatch,
        custom_objects_api,
        api_url="http://sb-timeout.sandbox-router",
    )
    sandbox_client.api_ready_timeout = 0

    cleanup = MagicMock()
    monkeypatch.setattr(sandbox_client, "__exit__", cleanup)

    with pytest.raises(TimeoutError):
        sandbox_client._wait_for_api_ready()

    cleanup.assert_called_once_with(None, None, None)
