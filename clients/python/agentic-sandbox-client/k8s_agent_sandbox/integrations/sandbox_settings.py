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

from abc import (
    ABC,
    abstractmethod,
)
from dataclasses import dataclass

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.extensions.computer_use import ComputerUseSandbox


@dataclass
class BaseSandboxSettings(ABC):
    template_name: str
    namespace: str = "default"
    gateway_name: str | None = None
    gateway_namespace: str = "default"
    api_url: str | None = None
    sandbox_ready_timeout: int = 180
    gateway_ready_timeout: int = 180
    port_forward_ready_timeout: int = 30
    server_port: int = 8888

    @abstractmethod
    def create_client(self) -> SandboxClient:
        """Creates an instance of client class"""


class SandboxSettings(BaseSandboxSettings):
    """
    A container class that stores all settings required for a creation
    of a particular agent sandbox. Its constructor signature is identical
    to 'k8s_agent_sandbox.SandboxClient'.
    """

    def create_client(self) -> SandboxClient:
        """Creates an instance of the 'SandboxClient' class"""

        return SandboxClient(
            self.template_name,
            namespace=self.namespace,
            gateway_name=self.gateway_name,
            gateway_namespace=self.gateway_namespace,
            api_url=self.api_url,
            server_port=self.server_port,
            sandbox_ready_timeout=self.sandbox_ready_timeout,
            gateway_ready_timeout=self.gateway_ready_timeout,
            port_forward_ready_timeout=self.port_forward_ready_timeout,
        )


class ComputerUseSandboxSettings(BaseSandboxSettings):
    """
    A container class that stores all settings required for a creation
    of a Computer Use agent sandbox. Its constructor signature is identical
    to 'k8s_agent_sandbox.extensions.ComputerUseSandbox'.
    """

    def create_client(self) -> ComputerUseSandbox:
        return ComputerUseSandbox(
            self.template_name,
            namespace=self.namespace,
            server_port=self.server_port,
        )
