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

"""``BaseSandboxClient`` implementation for Kubernetes Agent Sandbox."""

from __future__ import annotations

import logging
import uuid
from typing import Any

from agents.sandbox.manifest import Manifest
from agents.sandbox.session import SandboxSession, SandboxSessionState
from agents.sandbox.session.dependencies import Dependencies
from agents.sandbox.session.manager import Instrumentation
from agents.sandbox.session.sandbox_client import BaseSandboxClient
from agents.sandbox.snapshot import SnapshotBase, SnapshotSpec, resolve_snapshot
from k8s_agent_sandbox.exceptions import SandboxNotFoundError

from .options import K8sSandboxClientOptions, K8sSandboxSessionState
from .session import K8sSandboxSession
from .transport import K8sHttpTransport

logger = logging.getLogger(__name__)


class K8sSandboxClient(BaseSandboxClient[K8sSandboxClientOptions]):
    """Runs SandboxAgent workloads on Kubernetes Agent Sandbox.

    Takes an already-configured ``AsyncSandboxClient`` — the same shape as
    ``DockerSandboxClient(docker_from_env())`` — so cluster connectivity
    (direct/gateway/in-cluster) stays the caller's decision.

    Construct the underlying client with ``cleanup=False``: its default ``atexit`` hook
    deletes every tracked sandbox on process exit, which would destroy sandboxes that a
    later process still intends to ``resume()``. ``shutdown_after_seconds`` on the
    options is the leak backstop instead.
    """

    backend_id = "k8s"

    def __init__(
        self,
        sandbox_client: Any,
        *,
        instrumentation: Instrumentation | None = None,
        dependencies: Dependencies | None = None,
    ) -> None:
        super().__init__()
        self._sandbox_client = sandbox_client
        self._instrumentation = instrumentation or Instrumentation()
        self._dependencies = dependencies

    async def create(
        self,
        *,
        snapshot: SnapshotSpec | SnapshotBase | None = None,
        manifest: Manifest | None = None,
        options: K8sSandboxClientOptions,
    ) -> SandboxSession:
        manifest = manifest or Manifest()
        session_id = uuid.uuid4()

        sandbox = await self._create_sandbox(options)
        state = K8sSandboxSessionState(
            session_id=session_id,
            manifest=manifest,
            snapshot=resolve_snapshot(snapshot, str(session_id)),
            exposed_ports=options.exposed_ports,
            claim_name=sandbox.claim_name,
            sandbox_id=sandbox.sandbox_id,
            namespace=options.namespace,
            warm_pool=options.warm_pool,
            file_transfer=options.file_transfer,
            read_fallback=options.read_fallback,
            exec_timeout_default_s=options.exec_timeout_default_s,
            exposed_port_host=_exposed_port_host(options, sandbox.sandbox_id),
            exposed_port_host_override=options.exposed_port_host,
            sandbox_ready_timeout=options.sandbox_ready_timeout,
            shutdown_after_seconds=options.shutdown_after_seconds,
            labels=options.labels,
            pod_labels=options.pod_labels,
        )
        return self._wrap_session(
            self._build_session(sandbox, state), instrumentation=self._instrumentation
        )

    async def delete(self, session: SandboxSession) -> SandboxSession:
        # `_inner` belongs to the SDK's session wrapper, which only create()/resume() hand
        # out. Reaching for it directly would raise AttributeError on anything else --
        # including a bare K8sSandboxSession -- before the check below could say why.
        inner = getattr(session, "_inner", None)
        if not isinstance(inner, K8sSandboxSession):
            raise TypeError(
                "K8sSandboxClient.delete expects a K8sSandboxSession from create() or resume()"
            )

        try:
            await inner.shutdown()
        except Exception:
            # Teardown is best-effort; the claim deletion below is what actually frees
            # the pod and its PVC.
            logger.warning(
                "sandbox teardown failed during delete for %s; continuing",
                inner.state.claim_name,
                exc_info=True,
            )

        # delete_sandbox() logs a failed deletion and returns normally, reporting a leaked
        # pod and PVC as a clean delete. terminate() does the same work and raises.
        try:
            sandbox = await self._sandbox_client.get_sandbox(
                inner.state.claim_name, namespace=inner.state.namespace
            )
        except SandboxNotFoundError:
            # Claim already gone: nothing left to free. Anything else propagates.
            return session

        await sandbox.terminate()
        return session

    async def resume(self, state: SandboxSessionState) -> SandboxSession:
        if not isinstance(state, K8sSandboxSessionState):
            raise TypeError("K8sSandboxClient.resume expects a K8sSandboxSessionState")
        state.assert_path_grants_rebound()

        sandbox = await self._reattach(state)
        preserved = sandbox is not None

        if sandbox is None:
            # The claim is gone (TTL expiry, node loss, namespace cleanup). Provision a
            # replacement; start() hydrates its workspace from state.snapshot.
            sandbox = await self._create_sandbox(_options_from_state(state))
            state.claim_name = sandbox.claim_name
            state.sandbox_id = sandbox.sandbox_id
            state.workspace_root_ready = False
            if state.exposed_port_host:
                # A pinned host is the caller's routing decision and still holds. A derived
                # one named the old sandbox, so it has to follow the new one.
                state.exposed_port_host = state.exposed_port_host_override or _in_cluster_host(
                    sandbox.sandbox_id, state.namespace
                )

        inner = self._build_session(sandbox, state)
        inner._set_start_state_preserved(preserved)
        return self._wrap_session(inner, instrumentation=self._instrumentation)

    def deserialize_session_state(self, payload: dict[str, object]) -> SandboxSessionState:
        return self._deserialize_session_state_payload(payload, K8sSandboxSessionState)

    # -- internals ---------------------------------------------------------------

    async def _create_sandbox(self, options: K8sSandboxClientOptions) -> Any:
        return await self._sandbox_client.create_sandbox(
            options.warm_pool,
            namespace=options.namespace,
            sandbox_ready_timeout=options.sandbox_ready_timeout,
            labels=options.labels,
            shutdown_after_seconds=options.shutdown_after_seconds,
            pod_labels=options.pod_labels,
        )

    async def _reattach(self, state: K8sSandboxSessionState) -> Any | None:
        try:
            return await self._sandbox_client.get_sandbox(
                state.claim_name,
                namespace=state.namespace,
                warmpool_name=state.warm_pool,
            )
        except ValueError:
            # A warm-pool mismatch is a deliberate refusal by the client, not a missing
            # sandbox. Surface it rather than silently provisioning a replacement.
            raise
        except SandboxNotFoundError:
            # The only failure that justifies provisioning a replacement. Anything else —
            # an unreachable API server, a bug in this provider — would orphan a sandbox
            # that is still running, so it propagates.
            return None

    def _build_session(self, sandbox: Any, state: K8sSandboxSessionState) -> K8sSandboxSession:
        transport = K8sHttpTransport(
            sandbox,
            file_transfer=state.file_transfer,
            default_timeout_s=state.exec_timeout_default_s,
        )
        return K8sSandboxSession(transport=transport, state=state)


def _options_from_state(state: K8sSandboxSessionState) -> K8sSandboxClientOptions:
    """Rebuild the creation options a replacement sandbox has to be provisioned with."""

    return K8sSandboxClientOptions(
        warm_pool=state.warm_pool,
        namespace=state.namespace,
        sandbox_ready_timeout=state.sandbox_ready_timeout,
        shutdown_after_seconds=state.shutdown_after_seconds,
        exposed_ports=state.exposed_ports,
        exposed_port_host=state.exposed_port_host_override,
        labels=state.labels,
        pod_labels=state.pod_labels,
        file_transfer=state.file_transfer,
        read_fallback=state.read_fallback,
        exec_timeout_default_s=state.exec_timeout_default_s,
    )


def _exposed_port_host(options: K8sSandboxClientOptions, sandbox_id: str) -> str | None:
    if not options.exposed_ports:
        return None
    if options.exposed_port_host:
        return options.exposed_port_host
    return _in_cluster_host(sandbox_id, options.namespace)


def _in_cluster_host(sandbox_id: str, namespace: str) -> str:
    return f"{sandbox_id}.{namespace}.svc.cluster.local"
