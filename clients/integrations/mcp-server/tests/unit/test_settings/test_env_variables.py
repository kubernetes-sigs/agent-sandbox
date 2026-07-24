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

from k8s_agent_sandbox_mcp_server.settings import (
    Settings,
    DirectConnectionConfig,
    GatewayConnectionConfig,
    InClusterConnectionConfig,
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
