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

"""Kubernetes Agent Sandbox provider for the OpenAI Agents SDK.

Importing this package registers the ``"k8s"`` options and session-state discriminators
with the SDK's registries, which is what lets a persisted session round-trip.
"""

from .client import K8sSandboxClient
from .options import K8sSandboxClientOptions, K8sSandboxSessionState
from .session import K8sSandboxSession
from .transport import K8sHttpTransport, SandboxTransport

__all__ = [
    "K8sHttpTransport",
    "K8sSandboxClient",
    "K8sSandboxClientOptions",
    "K8sSandboxSession",
    "K8sSandboxSessionState",
    "SandboxTransport",
]
