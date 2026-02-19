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

from unittest.mock import MagicMock

import pytest

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox import sandbox_client as sandbox_client_module


def _build_client(
    monkeypatch: pytest.MonkeyPatch,
    custom_objects_api: MagicMock,
    template_name: str = "python-template",
    claim_name: str | None = None,
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
