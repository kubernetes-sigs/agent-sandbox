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

"""Handles for the streaming sandboxd ProcessService.Start RPC.

Wraps a server-streaming call as an iterator of typed events
(``ProcessOutputEvent`` then a terminal ``ProcessExitEvent``) and exposes
``signal()`` for POSIX cancellation via ProcessService.SendSignal. The handle
owns the ``process_id`` so callers never handle the raw id, mirroring the Go
SDK's ``ProcessHandle`` and the TypeScript SDK's ``ProcessHandle``.
"""

from typing import Any, AsyncIterator, Iterator, cast

from k8s_agent_sandbox.models import (
    ProcessEvent,
    ProcessExitEvent,
    ProcessOutputEvent,
)

# sandboxd's Signal enum accepts only these three. Names match the Go SDK's
# ProcessSignal constants and the TypeScript SDK's ProcessSignal union; the
# int forms let callers pass Python's own signal.SIGINT and friends.
_SIGNAL_NAMES = {"SIGINT": 2, "SIGTERM": 15, "SIGKILL": 9}


def _to_proto_signal(proto_module: Any, sig: int | str) -> int:
    """Map a signal name or number to the proto Signal enum.

    Accepts the cross-SDK spellings ("SIGINT") as well as an int or
    ``signal.Signals``, so Python callers can pass ``signal.SIGINT`` directly.
    """
    if isinstance(sig, str):
        if sig not in _SIGNAL_NAMES:
            raise ValueError(
                f"unsupported signal {sig!r}; sandboxd accepts "
                f"{', '.join(sorted(_SIGNAL_NAMES))}"
            )
        val = _SIGNAL_NAMES[sig]
    else:
        try:
            val = int(sig)
        except (TypeError, ValueError) as e:
            raise ValueError(
                f"signal must be a name, int or signal.Signals; got {sig!r}"
            ) from e
    if val == 2:
        return cast(int, proto_module.SIGNAL_SIGINT)
    if val == 9:
        return cast(int, proto_module.SIGNAL_SIGKILL)
    if val == 15:
        return cast(int, proto_module.SIGNAL_SIGTERM)
    raise ValueError(
        f"unsupported signal {sig!r}; sandboxd accepts SIGINT (2), SIGKILL (9), SIGTERM (15)"
    )


def _read_init(response: Any) -> int:
    """Validate the stream's first message and return its process id.

    sandboxd always sends Init first. Treating anything else as a protocol
    error here, rather than surfacing it as an event, is what lets
    ``process_id`` be set before ``start()`` returns.
    """
    if response.WhichOneof("event") != "init" or not response.init.process_id:
        raise RuntimeError(
            "sandboxd protocol error: expected an Init event with a nonzero "
            "process ID as the first message of ProcessService.Start"
        )
    return cast(int, response.init.process_id)


def _to_event(response: Any, exit_seen: bool) -> tuple[ProcessEvent, bool]:
    """Translate one StartResponse into a typed event.

    Rejects the protocol violations the Go SDK also rejects: anything after
    Exit (a duplicate Exit included), a second Init, and a frame with no
    recognized event. Returns the event and whether Exit has now been seen.

    Go additionally guards an Exit whose payload pointer is nil; that is not
    representable here, because an empty Exit submessage serializes
    identically to exit_code=0.
    """
    if exit_seen:
        raise RuntimeError("sandboxd protocol error: an event arrived after Exit")
    which = response.WhichOneof("event")
    if which == "stdout":
        return ProcessOutputEvent(type="stdout", data=response.stdout), False
    if which == "stderr":
        return ProcessOutputEvent(type="stderr", data=response.stderr), False
    if which == "exit":
        return ProcessExitEvent(exit_code=response.exit.exit_code), True
    if which == "init":
        # start() already consumed the one Init sandboxd sends.
        raise RuntimeError(
            "sandboxd protocol error: a second Init event arrived mid-stream")
    raise RuntimeError(
        "sandboxd protocol error: stream carried an unrecognized event")


_EMPTY_STREAM = (
    "sandboxd protocol error: ProcessService.Start closed without sending an "
    "Init event"
)


def _cancel_quietly(stream_call: Any) -> None:
    """Cancel a stream no handle owns yet.

    start() has not returned, so the caller has nothing to close(); without
    this the RPC stays open until garbage collection. A failure cancelling
    must not mask the error that got us here.
    """
    try:
        stream_call.cancel()
    except Exception:
        pass


def _signal_error(exc: Any, code: Any, process_id: int) -> Exception:
    """Translate a SendSignal gRPC error into the SDK's exception taxonomy."""
    import grpc

    if code == grpc.StatusCode.NOT_FOUND:
        return LookupError(f"process {process_id} not found: {exc.details()}")
    if code == grpc.StatusCode.PERMISSION_DENIED:
        return PermissionError(
            f"not permitted to signal process {process_id}: {exc.details()}"
        )
    return RuntimeError(f"sandboxd SendSignal failed ({code}): {exc.details()}")


class ProcessHandle:
    """Handle to an in-flight ProcessService.Start streaming call.

    Instances are produced by :meth:`CommandExecutor.start`. Iterate
    :meth:`events` to consume output; :attr:`process_id` is populated before
    the handle is returned, so :meth:`signal` works immediately without first
    consuming an event.

    Use as a context manager to guarantee the underlying stream is cancelled
    if the caller abandons iteration early.
    """

    def __init__(self, stub: Any, proto_module: Any, stream_call: Any,
                 process_id: int, iterator: Any = None) -> None:
        self._stub = stub
        self._proto = proto_module
        self._stream = stream_call
        # One iterator for the life of the handle: events() must resume after
        # the Init message from_stream already consumed, not restart the
        # stream.
        self._iter = iter(stream_call) if iterator is None else iterator
        self.process_id = process_id
        self._finished = False

    @classmethod
    def from_stream(cls, stub: Any, proto_module: Any,
                    stream_call: Any) -> "ProcessHandle":
        """Consume the stream's Init event and build the handle around it.

        gRPC errors propagate unwrapped: Start resolves them at the call site,
        where the connector is available to invalidate a dead transport.
        """
        iterator = iter(stream_call)
        try:
            first = next(iterator, None)
            if first is None:
                raise RuntimeError(_EMPTY_STREAM)
            process_id = _read_init(first)
        except BaseException:
            _cancel_quietly(stream_call)
            raise
        return cls(stub, proto_module, stream_call, process_id, iterator)

    def __enter__(self) -> "ProcessHandle":
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        self.close()

    def events(self) -> Iterator[ProcessEvent]:
        """Yield typed events until the process exits or the stream is cancelled."""
        import grpc

        try:
            for response in self._iter:
                event, self._finished = _to_event(response, self._finished)
                yield event
        except grpc.RpcError as e:
            rpc_error = cast("grpc.Call", e)
            raise RuntimeError(
                "sandboxd process stream failed "
                f"({rpc_error.code()}): {rpc_error.details()}"
            ) from e
        if not self._finished:
            raise RuntimeError(
                "sandboxd protocol error: the process stream ended without an "
                "Exit event")

    def signal(self, sig: int | str) -> None:
        """Deliver ``sig`` (SIGINT/SIGTERM/SIGKILL) to the running process."""
        import grpc

        request = self._proto.SendSignalRequest(
            process_id=self.process_id, signal=_to_proto_signal(self._proto, sig)
        )
        try:
            self._stub.SendSignal(request)
        except grpc.RpcError as e:
            rpc_error = cast("grpc.Call", e)
            raise _signal_error(
                rpc_error, rpc_error.code(), self.process_id) from e

    def close(self) -> None:
        """Cancel the underlying stream if it is still open."""
        if self._finished:
            return
        try:
            self._stream.cancel()
        except Exception:
            pass


class AsyncProcessHandle:
    """Async mirror of :class:`ProcessHandle` for ``grpc.aio`` streaming calls."""

    def __init__(self, stub: Any, proto_module: Any, stream_call: Any,
                 process_id: int) -> None:
        self._stub = stub
        self._proto = proto_module
        self._stream = stream_call
        self.process_id = process_id
        self._finished = False

    @classmethod
    async def from_stream(
        cls, stub: Any, proto_module: Any, stream_call: Any
    ) -> "AsyncProcessHandle":
        """Consume the stream's Init event and build the handle around it.

        gRPC errors propagate unwrapped: Start resolves them at the call site,
        where the connector is available to invalidate a dead transport.
        """
        import grpc

        try:
            first = await stream_call.read()
            if first is grpc.aio.EOF:
                raise RuntimeError(_EMPTY_STREAM)
            process_id = _read_init(first)
        except BaseException:
            _cancel_quietly(stream_call)
            raise
        return cls(stub, proto_module, stream_call, process_id)

    async def __aenter__(self) -> "AsyncProcessHandle":
        return self

    async def __aexit__(self, exc_type: Any, exc_val: Any,
                        exc_tb: Any) -> None:
        await self.close()

    async def events(self) -> AsyncIterator[ProcessEvent]:
        """Yield typed events until the process exits or the stream is cancelled."""
        import grpc

        # read() rather than `async for`: from_stream already consumed Init
        # with read(), and grpc.aio raises UsageError when the iterator and
        # read/write APIs are mixed on one RPC.
        try:
            while True:
                response = await self._stream.read()
                if response is grpc.aio.EOF:
                    break
                event, self._finished = _to_event(response, self._finished)
                yield event
        except grpc.aio.AioRpcError as e:
            raise RuntimeError(
                f"sandboxd process stream failed ({e.code()}): {e.details()}"
            ) from e
        if not self._finished:
            raise RuntimeError(
                "sandboxd protocol error: the process stream ended without an "
                "Exit event")

    async def signal(self, sig: int | str) -> None:
        """Deliver ``sig`` (SIGINT/SIGTERM/SIGKILL) to the running process."""
        import grpc

        request = self._proto.SendSignalRequest(
            process_id=self.process_id, signal=_to_proto_signal(self._proto, sig)
        )
        try:
            await self._stub.SendSignal(request)
        except grpc.aio.AioRpcError as e:
            raise _signal_error(e, e.code(), self.process_id) from e

    async def close(self) -> None:
        """Cancel the underlying stream if it is still open."""
        if self._finished:
            return
        try:
            self._stream.cancel()
        except Exception:
            pass
