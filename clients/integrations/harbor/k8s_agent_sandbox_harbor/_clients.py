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

"""Process-wide Kubernetes clients, shared by every trial of a Harbor job.

Harbor creates one environment object per trial and runs many at once. The
expensive parts, kubeconfig loading, the async SDK client with its watch
connections, the websocket thread pool, are built once per (context,
namespace) and reused. The SDK client runs with ``cleanup=False``: trials
delete their own claims, and an exit hook must not touch another process's.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
from dataclasses import dataclass
from typing import Any

from kubernetes import client as sync_client
from kubernetes import config as sync_config
from kubernetes_asyncio import client as async_client
from kubernetes_asyncio import config as async_config

from .options import EnvironmentOptions
from .templates import TemplateStore
from .transport.podexec import ThreadLocalCoreV1

logger = logging.getLogger(__name__)


@dataclass
class Clients:
    sandbox_client: Any
    """``k8s_agent_sandbox.AsyncSandboxClient`` bound to the context."""
    custom_objects: Any
    """``kubernetes_asyncio`` ``CustomObjectsApi`` from the same API client."""
    core_v1_factory: ThreadLocalCoreV1
    """Sync ``CoreV1Api`` per thread, for the exec websockets."""
    executor: concurrent.futures.ThreadPoolExecutor
    templates: TemplateStore
    api_client: Any = None
    """The ``kubernetes_asyncio`` ``ApiClient`` behind the two clients above."""


_clients: dict[tuple[str | None, str], Clients] = {}
_lock: asyncio.Lock | None = None


def _sync_configuration(context: str | None) -> sync_client.Configuration:
    configuration = sync_client.Configuration()
    try:
        sync_config.load_incluster_config(client_configuration=configuration)
    except sync_config.ConfigException:
        sync_config.load_kube_config(context=context, client_configuration=configuration)
    return configuration


async def _async_api_client(context: str | None) -> async_client.ApiClient:
    configuration = async_client.Configuration()
    try:
        async_config.load_incluster_config(client_configuration=configuration)
    except async_config.ConfigException:
        await async_config.load_kube_config(context=context, client_configuration=configuration)
    return async_client.ApiClient(configuration)


async def get_clients(options: EnvironmentOptions) -> Clients:
    """The shared clients for ``options``' context and namespace."""
    global _lock
    if _lock is None:
        _lock = asyncio.Lock()
    key = (options.kube_context, options.namespace)
    async with _lock:
        existing = _clients.get(key)
        if existing is not None:
            return existing
        # Imported here so the module imports without the SDK's async extra
        # until an environment actually starts.
        from k8s_agent_sandbox import AsyncSandboxClient
        from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

        api_client = await _async_api_client(options.kube_context)
        sandbox_client = AsyncSandboxClient(
            # Exec and files go through the pod exec API, not the SDK's
            # connectors; this config only keeps the client from opening a
            # port-forward to the sandbox router.
            connection_config=SandboxInClusterConnectionConfig(),
            cleanup=False,
            api_client=api_client,
        )
        custom_objects = async_client.CustomObjectsApi(api_client)
        executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=options.exec_threads, thread_name_prefix="k8s-sandbox-exec")
        clients = Clients(
            sandbox_client=sandbox_client,
            custom_objects=custom_objects,
            core_v1_factory=ThreadLocalCoreV1(_sync_configuration(options.kube_context)),
            executor=executor,
            templates=TemplateStore(custom_objects, options.namespace, options, logger),
            api_client=api_client,
        )
        _clients[key] = clients
        return clients


async def close_clients() -> None:
    """Close every shared client; for the end of a process that owns the loop
    (the cleanup command). Harbor's own exit closes its loop without this."""
    for clients in list(_clients.values()):
        close = getattr(clients.sandbox_client, "close", None)
        if close is not None:
            try:
                await close()
            except Exception:  # noqa: BLE001 - best effort at exit
                pass
        if clients.api_client is not None:
            try:
                await clients.api_client.close()
            except Exception:  # noqa: BLE001
                pass
        clients.executor.shutdown(wait=False)
    _clients.clear()


def reset_clients_for_tests() -> None:
    _clients.clear()
