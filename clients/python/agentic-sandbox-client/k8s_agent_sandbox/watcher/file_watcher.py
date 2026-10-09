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

"""Synchronous filesystem event streaming for sandboxd runtimes."""

from __future__ import annotations

from typing import Any, Iterator

from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.models import FileEvent
from k8s_agent_sandbox.trace_manager import trace as _otel_trace

# Proto enum value → SDK string type. Populated lazily to avoid importing
# gRPC at module load (the grpc extra may not be installed).
_EVENT_TYPE_NAMES: dict[int, str] | None = None


def _event_type_name(value: int) -> str:
    """Map a FileEventType proto enum value to its lowercase SDK string."""
    global _EVENT_TYPE_NAMES  # noqa: PLW0603
    if _EVENT_TYPE_NAMES is None:
        from k8s_agent_sandbox.watcher._watcher_stubs import watcher_pb2

        _EVENT_TYPE_NAMES = {
            watcher_pb2.FILE_EVENT_TYPE_CREATE: "create",
            watcher_pb2.FILE_EVENT_TYPE_WRITE: "write",
            watcher_pb2.FILE_EVENT_TYPE_REMOVE: "remove",
            watcher_pb2.FILE_EVENT_TYPE_RENAME: "rename",
            watcher_pb2.FILE_EVENT_TYPE_CHMOD: "chmod",
            watcher_pb2.FILE_EVENT_TYPE_ERROR: "error",
        }
    return _EVENT_TYPE_NAMES.get(value, f"unknown({value})")


class FileWatcher:
    """Stream filesystem events from a sandboxd-managed directory.

    Uses the ``watcher.v1.FileWatcherService`` gRPC API. Requires the
    ``grpc`` extra and a sandboxd runtime; raises :class:`ImportError`
    on construction when ``grpcio`` is unavailable, and :class:`RuntimeError`
    on :meth:`watch` when the connected runtime is not sandboxd.
    """

    def __init__(
        self, connector: SandboxConnector, tracer: Any, trace_service_name: str
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    def watch(
        self,
        path: str = ".",
        recursive: bool = False,
    ) -> Iterator[FileEvent]:
        """Subscribe to filesystem events for *path*.

        Yields :class:`~k8s_agent_sandbox.models.FileEvent` instances as
        changes occur. The iterator terminates when the underlying gRPC
        stream ends (server closes, the watched directory is removed, or
        an error occurs).

        To stop the watch early, close the underlying sandbox connection
        (e.g. ``sandbox.close_connection()``) which terminates the gRPC
        stream and ends the iteration. Unlike the async counterpart, the
        synchronous iterator blocks on the gRPC channel and cannot be
        interrupted from another thread without closing the connection.

        Args:
            path: Sandbox-relative directory path to watch. Defaults to
                the sandbox root (``"."``).
            recursive: If ``True``, also watch subdirectories. New
                subdirectories created during the watch are automatically
                added; removed ones are dropped.

        Yields:
            FileEvent instances for each observed change.

        Raises:
            RuntimeError: If the connected runtime is not sandboxd, or if
                the gRPC stream fails.
            ImportError: If grpcio is not installed.
        """
        # Manage the span inside the generator body rather than via
        # @trace_span: decorating a generator function opens/closes the
        # span when the generator object is *created*, not when iteration
        # runs, so the span would end before any event is yielded.
        # When tracing is disabled, self.tracer is None; fall back to the
        # (no-op) global tracer so start_span is always safe to call.
        tracer = self.tracer if self.tracer is not None else _otel_trace.get_tracer(
            self.trace_service_name or "sandbox-client"
        )
        span = tracer.start_span("watch")
        try:
            if span.is_recording():
                span.set_attribute("sandbox.path", path)
                span.set_attribute("sandbox.recursive", recursive)

            if not self.connector.is_sandboxd():
                raise RuntimeError(
                    "FileWatcher requires the sandboxd runtime; "
                    "the legacy python-runtime does not implement FileWatcherService."
                )

            try:
                import grpc

                from k8s_agent_sandbox.watcher._watcher_stubs import (
                    watcher_pb2,
                    watcher_pb2_grpc,
                )
            except ImportError as e:
                raise ImportError(
                    "FileWatcher requires gRPC support; install the "
                    "'grpc' extra: pip install k8s-agent-sandbox[grpc]"
                ) from e

            # Ensure the pod tunnel is established (and the gRPC target
            # published) before dialing.
            self.connector.connect()
            channel = self.connector.grpc_channel()
            stub = watcher_pb2_grpc.FileWatcherServiceStub(channel)
            request = watcher_pb2.WatchDirRequest(path=path, recursive=recursive)

            # For server-streaming RPCs, stub.WatchDir() returns the stream
            # immediately; gRPC errors surface during iteration, not at the
            # call site. Wrap both to be safe.
            try:
                stream = stub.WatchDir(request)
                for proto_event in stream:
                    yield FileEvent(
                        type=_event_type_name(proto_event.type),
                        path=proto_event.path,
                        old_path=proto_event.old_path,
                        error=proto_event.error,
                    )
            except grpc.RpcError as e:
                raise RuntimeError(
                    f"sandboxd file watcher service failed ({e.code()}): {e.details()}"
                ) from e
        except Exception:
            if span.is_recording():
                try:
                    from opentelemetry.trace import StatusCode

                    span.set_status(StatusCode.ERROR)
                except ImportError:
                    # opentelemetry is an optional dependency; the SDK
                    # falls back to a no-op MockSpan when it is absent,
                    # but test mocks (MagicMock) may still report
                    # is_recording() truthy — swallow the ImportError.
                    pass
            raise
        finally:
            span.end()
