# Copyright 2026 The Kubernetes Authors.
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

"""Asynchronous filesystem event streaming for sandboxd runtimes."""

from __future__ import annotations

from typing import Any, AsyncIterator

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.models import FileEvent
from k8s_agent_sandbox.trace_manager import trace
from k8s_agent_sandbox.watcher.file_watcher import _event_type_name


class AsyncFileWatcher:
    """Async stream of filesystem events from a sandboxd-managed directory.

    Uses the ``watcher.v1.FileWatcherService`` gRPC API through
    ``grpc.aio``. Mirrors :class:`FileWatcher` but yields events via an
    async iterator. Requires the ``grpc`` extra.
    """

    def __init__(
        self,
        connector: AsyncSandboxConnector,
        tracer: Any,
        trace_service_name: str,
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    async def watch(
        self,
        path: str = ".",
        recursive: bool = False,
    ) -> AsyncIterator[FileEvent]:
        """Subscribe to filesystem events for *path*.

        Yields :class:`~k8s_agent_sandbox.models.FileEvent` instances
        asynchronously. The iterator terminates when the underlying gRPC
        stream ends (server closes, client disconnects, or the watched
        directory is removed).

        Args:
            path: Sandbox-relative directory path to watch. Defaults to
                the sandbox root (``"."``).
            recursive: If ``True``, also watch subdirectories. New
                subdirectories created during the watch are automatically
                added; removed ones are dropped.

        Yields:
            FileEvent instances for each observed change.

        Raises:
            RuntimeError: If the connected runtime is not sandboxd.
            ImportError: If grpcio is not installed.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.path", path)
            span.set_attribute("sandbox.recursive", recursive)

        if not self.connector.is_sandboxd():
            raise RuntimeError(
                "AsyncFileWatcher requires the sandboxd runtime; "
                "the legacy python-runtime does not implement FileWatcherService."
            )

        try:
            import grpc  # noqa: F401

            from k8s_agent_sandbox.watcher._watcher_stubs import (
                watcher_pb2,
                watcher_pb2_grpc,
            )
        except ImportError as e:
            raise ImportError(
                "AsyncFileWatcher requires gRPC support; install the "
                "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
            ) from e

        # Ensure the pod tunnel is established.
        await self.connector.connect()
        channel = await self.connector.grpc_channel()
        stub = watcher_pb2_grpc.FileWatcherServiceStub(channel)
        request = watcher_pb2.WatchDirRequest(path=path, recursive=recursive)

        try:
            stream = stub.WatchDir(request)
        except grpc.RpcError as e:
            raise RuntimeError(
                f"sandboxd file watcher service failed ({e.code()}): {e.details()}"
            ) from e

        async for proto_event in stream:
            yield FileEvent(
                type=_event_type_name(proto_event.type),
                path=proto_event.path,
                old_path=proto_event.old_path,
                error=proto_event.error,
            )
