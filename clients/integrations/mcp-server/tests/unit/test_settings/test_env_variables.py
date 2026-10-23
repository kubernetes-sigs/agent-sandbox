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

import pytest
from pydantic import ValidationError

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox_mcp_server.settings import (
    Settings,
    DirectConnectionConfig,
    GatewayConnectionConfig,
    InClusterConnectionConfig,
    SandboxdConnectionConfig,
)


class TestDirectConnection:
    def test_env_vars(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "direct")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__API_URL", "http://some-url")
    
        settings = Settings()
    
        assert type(settings.connection) is DirectConnectionConfig
        assert settings.connection.api_url == "http://some-url"


class TestGatewayConnection:
    def test_env_vars(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "gateway")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__GATEWAY_NAME", "some-gateway")
    
        settings = Settings()
    
        assert type(settings.connection) is GatewayConnectionConfig 
        assert settings.connection.gateway_name == "some-gateway"

class TestInClusterConnection:
    def test_env_vars(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "in-cluster")
    
        settings = Settings()
    
        assert type(settings.connection) is InClusterConnectionConfig 


class TestSandboxdConnection:
    @pytest.mark.parametrize("mode", ["in-cluster-service", "in-cluster-pod-ip"])
    def test_env_vars(self, monkeypatch, mode):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "sandboxd-in-cluster")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__MODE", mode)

        settings = Settings()

        assert type(settings.connection) is SandboxdConnectionConfig
        assert settings.connection.mode == mode
        assert settings.connection.rest_port == 8080
        assert settings.connection.grpc_port == 9090

    def test_env_vars_ports(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "sandboxd-in-cluster")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__MODE", "in-cluster-pod-ip")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__REST_PORT", "18080")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__GRPC_PORT", "19090")

        settings = Settings()

        assert settings.connection.rest_port == 18080
        assert settings.connection.grpc_port == 19090

    def test_mode_is_required(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "sandboxd-in-cluster")

        with pytest.raises(ValidationError):
            Settings()

    def test_rejects_unknown_mode(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "sandboxd-in-cluster")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__MODE", "port-forward")

        with pytest.raises(ValidationError):
            Settings()

    def test_sdk_connector_uses_sandboxd(self, monkeypatch):
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__TYPE", "sandboxd-in-cluster")
        monkeypatch.setenv("K8S_SANDBOX_CONNECTION__MODE", "in-cluster-service")

        connector = AsyncSandboxConnector(
            sandbox_id="my-sandbox",
            namespace="my-namespace",
            connection_config=Settings().connection,
            k8s_helper=None,
        )

        assert connector.is_sandboxd()
        assert not connector.should_inject_router_headers()
