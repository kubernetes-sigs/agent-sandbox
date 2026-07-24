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

from typing import (
    Literal,
    Union,
)

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict
from k8s_agent_sandbox.models import (
    SandboxGatewayConnectionConfig,
    SandboxDirectConnectionConfig, 
    SandboxInClusterConnectionConfig,
)


class GatewayConnectionConfig(SandboxGatewayConnectionConfig):
    type: Literal["gateway"] = Field(default="gateway")


class DirectConnectionConfig(SandboxDirectConnectionConfig):
    type: Literal["direct"] = Field(default="direct")


class InClusterConnectionConfig(SandboxInClusterConnectionConfig):
    type: Literal["in-cluster"] = Field(default="in-cluster")


ConnectionType = Union[GatewayConnectionConfig, DirectConnectionConfig, InClusterConnectionConfig]

class Settings(BaseSettings):
    connection: ConnectionType = Field(default=..., discriminator='type')  
    session_id_label_key: str = Field(
        default="mcp.k8s-agent-sandbox/session-id",
    )

    model_config = SettingsConfigDict(
        env_file=".env",
        env_prefix="K8S_SANDBOX_",
        env_nested_delimiter="__",
    )
