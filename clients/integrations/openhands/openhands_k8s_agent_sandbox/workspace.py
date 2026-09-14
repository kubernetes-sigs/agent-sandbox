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

"""Agent-sandbox-backed workspace for the OpenHands agent SDK.

``AgentSandboxWorkspace`` is a provisioning-only ``RemoteWorkspace`` subclass:
instead of ``docker run`` + health-wait (``DockerWorkspace``), it claims a
**pre-warmed** pod from a ``SandboxWarmPool`` whose template already runs the
OpenHands agent-server image, and hands the pod's URL to the base class. All
workspace operations (bash, files, git) are inherited from ``RemoteWorkspace``,
which speaks HTTP to the agent-server — this module adds no execution plumbing.

The warm pool's SandboxTemplate must run the agent-server image with
``--host 0.0.0.0 --port <server_port>`` and a readinessProbe on
``/health:<server_port>`` so that a Ready pod == a healthy agent-server
(see ``configs/agent-server-template.yaml``).
"""

from __future__ import annotations

import time
from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _dist_version
from typing import Any
from urllib.request import Request, urlopen

import httpx
from pydantic import Field, PrivateAttr

from openhands.sdk.logger import get_logger
from openhands.sdk.workspace import RemoteWorkspace


logger = get_logger(__name__)


class AgentSandboxWorkspace(RemoteWorkspace):
    """Remote workspace bound to a pre-warmed agent-sandbox pod.

    Provisioning maps 1:1 onto ``DockerWorkspace``'s lifecycle, with the cold
    container start replaced by a warm-pool claim (a bind, not a boot):

    - ``model_post_init``: claim → resolve endpoint → health check → hand off
      to ``RemoteWorkspace``.
    - ``cleanup()`` (also via ``with`` / ``__del__``): delete the claim; the
      pool replenishes in the background. ``ttl_s`` backstops clients that die
      without cleanup.

    Example:
        >>> with AgentSandboxWorkspace(warmpool="agent-server-pool",
        ...                            namespace="openhands") as workspace:
        ...     result = workspace.execute_command("ls -la")

    Auth note: pre-warmed servers cannot receive a per-claim key, so all pods
    in a pool share the session key their template injects (env
    ``OH_SESSION_API_KEYS_0``, typically from one Secret). Pass that same value
    as ``api_key``; leave both unset only on a private, trusted network.
    """

    # Overridden parent defaults.
    working_dir: str = Field(
        default="/workspace",
        description="Working directory inside the sandbox pod.",
    )
    host: str = Field(
        default="",
        description=(
            "Agent-server URL. Always derived from the claimed sandbox once "
            "the claim binds; a caller-supplied value is replaced (with a "
            "warning). Use router_url or endpoint_template to reach pods "
            "through a gateway."
        ),
    )

    # Provisioning configuration.
    warmpool: str = Field(
        description="SandboxWarmPool holding pre-warmed agent-server pods."
    )
    namespace: str = Field(
        default="default", description="Kubernetes namespace of the warm pool."
    )
    server_port: int = Field(
        default=8000,
        ge=1,
        le=65535,
        description="Port the agent-server listens on inside the pod.",
    )
    endpoint_template: str | None = Field(
        default=None,
        description=(
            "Optional URL template for the agent-server endpoint, for gateway or "
            "proxied data paths. Supports {pod_ip}, {port}, {namespace}, "
            "{claim_name}, {sandbox_id}. Default: http://{pod_ip}:{port} "
            "(GKE pod IPs are VPC-routable). Mutually exclusive with router_url."
        ),
    )
    router_url: str | None = Field(
        default=None,
        description=(
            "Base URL of a sandbox-router deployment (see the client's "
            "sandbox-router/). When set, all traffic goes to the router with "
            "X-Sandbox-* routing headers (ID, namespace, port, pod IP) injected "
            "on every request — including the health check. Use when pod IPs "
            "are not routable from the client. Mutually exclusive with "
            "endpoint_template."
        ),
    )
    router_auth_token: str | None = Field(
        default=None,
        description=(
            "Bearer token for the sandbox-router (its ROUTER_AUTH_TOKEN). Sent "
            "as Authorization to the router, which strips it before forwarding "
            "— it never reaches the agent-server, so it composes with api_key."
        ),
    )
    claim_timeout_s: int = Field(
        default=60,
        gt=0,
        description="Seconds to wait for the claim to bind a Ready sandbox.",
    )
    health_check_timeout: float = Field(
        default=10.0,
        gt=0.0,
        description=(
            "Seconds to wait for /health. Deliberately short: a warm pod that "
            "is Ready but unhealthy is broken — fail fast and re-claim rather "
            "than wait out a Docker-style cold-boot budget."
        ),
    )
    ttl_s: int | None = Field(
        default=None,
        gt=0,
        description=(
            "Optional claim TTL (spec.lifecycle shutdownTime). The controller "
            "deletes the claim on expiry even if this process dies first."
        ),
    )
    claim_labels: dict[str, str] | None = Field(
        default=None, description="Labels for the SandboxClaim object."
    )
    check_server_version: bool = Field(
        default=True,
        description=(
            "After attach, compare the agent-server's reported version with "
            "the installed openhands-sdk and log a loud warning on skew (the "
            "SDK and server image are released together). Never fails the "
            "attach; disable for offline tests."
        ),
    )
    sandbox_client: Any = Field(
        default=None,
        exclude=True,
        description=(
            "Injected k8s_agent_sandbox.SandboxClient (or compatible: "
            "create_sandbox() returns an object with claim_name, sandbox_id, "
            "get_pod_ip() and terminate()). Built lazily with defaults when "
            "omitted; tests inject fakes. A returned object may set "
            "releases_on_terminate=False when its terminate() does not "
            "release the pod, so cleanup logs a detach rather than a release."
        ),
    )

    _sandbox: Any = PrivateAttr(default=None)
    _owns_client: bool = PrivateAttr(default=False)

    @property
    def _headers(self):
        """Session headers, plus router routing/auth headers in router mode.

        The router resolves the backend from X-Sandbox-* headers on EVERY
        request; the pod IP shortcut skips the per-sandbox Service DNS hop.
        The Bearer token authenticates to the router only — the router strips
        Authorization before forwarding, so it never shadows api_key.
        """
        headers = dict(super()._headers)
        if self.router_url and self._sandbox is not None:
            headers["X-Sandbox-ID"] = getattr(self._sandbox, "sandbox_id", "")
            headers["X-Sandbox-Namespace"] = self.namespace
            headers["X-Sandbox-Port"] = str(self.server_port)
            pod_ip = self._sandbox.get_pod_ip()
            if pod_ip:
                headers["X-Sandbox-Pod-IP"] = pod_ip
            if self.router_auth_token:
                headers["Authorization"] = f"Bearer {self.router_auth_token}"
        return headers

    def model_post_init(self, context: Any) -> None:
        """Claim a warm pod, point RemoteWorkspace at it, verify health."""
        if self.router_url and self.endpoint_template:
            raise ValueError(
                "router_url and endpoint_template are mutually exclusive"
            )
        client = self.sandbox_client
        if client is None:
            from k8s_agent_sandbox import SandboxClient

            client = SandboxClient()
            self.sandbox_client = client
            self._owns_client = True

        sandbox = client.create_sandbox(
            self.warmpool,
            namespace=self.namespace,
            sandbox_ready_timeout=self.claim_timeout_s,
            labels=self.claim_labels,
            shutdown_after_seconds=self.ttl_s,
        )
        self._sandbox = sandbox
        logger.info(
            "Claimed sandbox %s from warm pool %r (ns=%s)",
            getattr(sandbox, "claim_name", "?"),
            self.warmpool,
            self.namespace,
        )
        try:
            if self.host:
                logger.warning(
                    "host=%r is ignored: the endpoint is always derived from "
                    "the claimed sandbox (use router_url or endpoint_template "
                    "for gateway/proxied paths)",
                    self.host,
                )
            # Bypass validate-assignment plumbing when rewriting connection
            # fields mid-init (the same trick DockerWorkspace uses).
            object.__setattr__(self, "host", self._endpoint_url(sandbox))
            self._wait_for_health(timeout=self.health_check_timeout)
        except Exception:
            # Never leave an orphaned claim behind a failed init.
            self._terminate_sandbox()
            raise
        logger.info("Agent-sandbox workspace is ready at %s", self.host)
        super().model_post_init(context)
        if self.check_server_version:
            self._log_version_skew()

    def _log_version_skew(self) -> None:
        """Warn (never fail) when the server and SDK versions diverge.

        Only the failures an advisory probe is expected to hit stay quiet:
        transport/HTTP errors reaching ``/server_info``, a non-JSON body, or
        an SDK that isn't installed as a distribution. Anything else — an
        unexpected payload shape or a bug in this check — is logged at
        WARNING so it cannot silently disable the advisory on every attach.
        """
        try:
            info = self.get_server_info()
        except (httpx.HTTPError, ValueError) as e:
            # Connection refused / timeout, non-2xx, or a body that isn't JSON.
            logger.debug("server version check skipped: %s", e)
            return
        except Exception:  # noqa: BLE001 — advisory: never break the attach
            logger.warning("server version check failed unexpectedly", exc_info=True)
            return
        if not isinstance(info, dict):
            logger.warning(
                "server version check skipped: /server_info returned %s, "
                "expected a JSON object",
                type(info).__name__,
            )
            return
        server_version = str(info.get("version") or "")
        try:
            sdk_version = _dist_version("openhands-sdk")
        except PackageNotFoundError:
            logger.debug(
                "server version check skipped: openhands-sdk is not installed "
                "as a distribution"
            )
            return
        if server_version and server_version != sdk_version:
            logger.warning(
                "agent-server reports version %s but the installed "
                "openhands-sdk is %s — align the SandboxTemplate image "
                "tag with the SDK release to avoid protocol skew",
                server_version,
                sdk_version,
            )

    def _endpoint_url(self, sandbox: Any) -> str:
        if self.router_url:
            return self.router_url.rstrip("/")
        pod_ip = sandbox.get_pod_ip()
        if self.endpoint_template:
            return self.endpoint_template.format(
                pod_ip=pod_ip or "",
                port=self.server_port,
                namespace=self.namespace,
                claim_name=getattr(sandbox, "claim_name", ""),
                sandbox_id=getattr(sandbox, "sandbox_id", ""),
            )
        if not pod_ip:
            raise RuntimeError(
                f"sandbox {getattr(sandbox, 'claim_name', '?')!r} bound but has "
                "no pod IP; cannot build the agent-server endpoint (set "
                "endpoint_template for gateway/proxied data paths)"
            )
        return f"http://{pod_ip}:{self.server_port}"

    def _wait_for_health(self, *, timeout: float) -> None:
        """Poll GET /health until 2xx or the (short) budget elapses.

        In router mode the routing headers must ride along or the router
        rejects the probe with 400 before it ever reaches the sandbox.
        """
        health_url = f"{self.host.rstrip('/')}/health"
        deadline = time.monotonic() + timeout
        last_error: Exception | None = None
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            try:
                request = Request(health_url, headers=self._headers)
                # Bound each probe and the backoff by the remaining budget so
                # the caller's timeout is honored, not just approximated.
                with urlopen(request, timeout=min(1.0, remaining)) as resp:
                    if 200 <= getattr(resp, "status", 200) < 300:
                        return
            except Exception as e:  # noqa: BLE001 — retried until the deadline
                last_error = e
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            time.sleep(min(0.5, remaining))
        raise RuntimeError(
            f"agent-server at {health_url} failed to become healthy within "
            f"{timeout}s (pre-warmed pods should be healthy at claim time; "
            f"last error: {last_error!r})"
        )

    # ------------------------------------------------------------- lifecycle

    def __enter__(self) -> "AgentSandboxWorkspace":
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:  # type: ignore[no-untyped-def]
        self.cleanup()

    def __del__(self) -> None:
        # Guard against interpreter-shutdown states where pydantic private
        # attributes are already gone (same pattern as DockerWorkspace).
        try:
            object.__getattribute__(self, "__pydantic_private__")
        except AttributeError:
            return
        self.cleanup()

    def cleanup(self) -> None:
        """Delete the claim (the pool replenishes) and drop an owned client.

        A failed termination keeps the claim reference so a later cleanup()
        (or __del__) can retry — otherwise a claim with no ttl_s would leak
        and silently eat warm-pool capacity. The owned client is kept open
        while a retry is still possible.
        """
        self._terminate_sandbox()
        if self._sandbox is not None:
            return  # termination failed; keep the client for the retry
        if self._owns_client and self.sandbox_client is not None:
            close = getattr(self.sandbox_client, "close", None)
            if callable(close):
                try:
                    close()
                except Exception as e:  # noqa: BLE001 — best-effort teardown
                    logger.warning("Error closing sandbox client: %s", e)
            self.sandbox_client = None
            self._owns_client = False

    def _terminate_sandbox(self) -> None:
        sandbox = self._sandbox
        if sandbox is None:
            return
        # Capture before terminate(): the SDK clears identity fields on close.
        claim_name = getattr(sandbox, "claim_name", "?")
        try:
            sandbox.terminate()
        except Exception as e:  # noqa: BLE001 — retried on the next cleanup()
            logger.warning(
                "Failed to terminate sandbox %s: %s (kept for retry; ttl_s "
                "backstop applies if configured)",
                claim_name,
                e,
            )
            return
        # Clear only after success so a failed delete stays retryable.
        self._sandbox = None
        if getattr(sandbox, "releases_on_terminate", True):
            logger.info("Released sandbox claim %s", claim_name)
        else:
            # Injected view whose terminate() is a no-op on the pod (e.g. a
            # fleet-bound handle): we only detached; its owner releases it.
            logger.info(
                "Detached from sandbox %s (its owner releases the claim)",
                claim_name,
            )
