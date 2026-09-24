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

"""Options and session state for the Kubernetes Agent Sandbox provider.

Both types are pydantic models with a ``type`` discriminator. The SDK registers
subclasses in a global registry on import, which is how a persisted session state
round-trips back to the right provider. That registry has no entry-point
discovery: this package must be imported before
``BaseSandboxClient.deserialize_session_state`` can resolve ``"k8s"``.
"""

from __future__ import annotations

from typing import Literal

from agents.sandbox.session import SandboxSessionState
from agents.sandbox.session.sandbox_client import BaseSandboxClientOptions

# How file bytes move in and out of the sandbox.
#
# "http" uses the in-pod server's upload/download endpoints, which are bytes-clean.
# "exec" base64-encodes through the JSON ``execute`` endpoint: slower and bounded by
# command-length limits, but it works when the in-pod server rejects absolute paths
# (its client-side sanitizer strips a leading "/") or is not reachable for file I/O.
FileTransfer = Literal["http", "exec"]


class K8sSandboxClientOptions(BaseSandboxClientOptions):
    """Per-run settings for :class:`~openai_agents_k8s_sandbox.client.K8sSandboxClient`."""

    type: Literal["k8s"] = "k8s"

    warm_pool: str
    """Name of the ``SandboxWarmPool`` to claim from."""

    namespace: str = "default"
    sandbox_ready_timeout: int = 180

    shutdown_after_seconds: int | None = 3600
    """TTL backstop. Sets ``shutdownTime``/``shutdownPolicy: Delete`` on the claim so a
    crashed run cannot leak a pod. ``None`` disables it."""

    exposed_ports: tuple[int, ...] = ()

    exposed_port_host: str | None = None
    """Host that :meth:`resolve_exposed_port` reports. Defaults to the sandbox's stable
    in-cluster DNS name, which is only reachable from inside the cluster."""

    labels: dict[str, str] | None = None
    """Labels on the ``SandboxClaim`` object."""

    pod_labels: dict[str, str] | None = None
    """Labels stamped onto the running pod (readable inside the sandbox via the Downward API)."""

    file_transfer: FileTransfer = "http"

    read_fallback: bool = True
    """Whether a failed download retries over exec. ``False`` surfaces the transport error
    instead, for callers who would rather see a broken endpoint than silently read every
    file over the slower, ARG_MAX-bounded path."""

    exec_timeout_default_s: float = 300.0
    """Timeout applied when the SDK passes ``timeout=None``. The in-pod HTTP API has no
    unbounded mode, so a finite default is required."""


class K8sSandboxSessionState(SandboxSessionState):
    """Everything needed to reattach to a sandbox in a later process.

    Every field below the connection details exists because ``resume()`` may have to
    provision a *replacement* sandbox, and that replacement has to be configured the way
    the original was. Anything not recorded here is silently reset to a class default.
    All of them are optional so payloads written by an earlier version still deserialize.
    """

    type: Literal["k8s"] = "k8s"

    claim_name: str
    sandbox_id: str
    namespace: str
    warm_pool: str
    file_transfer: FileTransfer = "http"
    read_fallback: bool = True
    exec_timeout_default_s: float = 300.0
    exposed_port_host: str | None = None
    """Resolved host for :meth:`resolve_exposed_port`, derived unless pinned below."""

    exposed_port_host_override: str | None = None
    """The caller's explicit ``exposed_port_host``, if any. Survives a replacement; the
    derived in-cluster name does not, because it names a sandbox that no longer exists."""

    sandbox_ready_timeout: int = 180
    shutdown_after_seconds: int | None = 3600
    labels: dict[str, str] | None = None
    pod_labels: dict[str, str] | None = None
