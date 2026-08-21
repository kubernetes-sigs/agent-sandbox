"""Kubernetes Agent Sandbox provider for the OpenAI Agents SDK.

Importing this package registers the ``"k8s"`` options and session-state discriminators
with the SDK's registries, which is what lets a persisted session round-trip.
"""

from .client import K8sSandboxClient
from .options import K8sSandboxClientOptions, K8sSandboxSessionState
from .session import K8sSandboxSession
from .transport import K8sHttpTransport, SandboxTransport

__all__ = [
    "K8sSandboxClient",
    "K8sSandboxClientOptions",
    "K8sSandboxSession",
    "K8sSandboxSessionState",
    "K8sHttpTransport",
    "SandboxTransport",
]
