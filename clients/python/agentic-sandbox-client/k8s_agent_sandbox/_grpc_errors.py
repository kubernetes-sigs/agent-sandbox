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

"""Translation of sandboxd gRPC statuses into the SDK's exception taxonomy.

Classification keys off the status *name* rather than identity against
``grpc.StatusCode``, so this module needs no grpcio import"""

from typing import Any

from k8s_agent_sandbox.constants import ERROR_DETAIL_MAX_BYTES
from k8s_agent_sandbox.exceptions import (
    SandboxConnectionError,
    SandboxdPermissionError,
    SandboxdProcessNotFoundError,
    SandboxdRpcError,
    SandboxError,
    SandboxTimeoutError,
)


def _truncate_detail(text: str) -> str:
    """Bound untrusted diagnostic text before it reaches an exception.

    Status details come from the server and have no length contract, so they
    are capped before being put in a message or on ``detail``.
    """
    encoded = text.encode("utf-8")
    if len(encoded) <= ERROR_DETAIL_MAX_BYTES:
        return text
    # Cut the encoded form so the cap is a byte budget, then drop the partial
    # character a cut mid-sequence leaves behind.
    kept = encoded[:ERROR_DETAIL_MAX_BYTES].decode("utf-8", errors="ignore")
    return f"{kept}... [truncated]"


def _status(exc: Any) -> tuple[Any, str]:
    """Read an RpcError's status code and details without trusting the stubs.

    grpcio declares code()/details() on grpc.Call but not consistently on
    RpcError across versions; the rendezvous objects raised by streaming calls
    implement both. Read them defensively to stay stub-version independent.
    """
    code = getattr(exc, "code", lambda: None)()
    details = getattr(exc, "details", lambda: str(exc))()
    return code, details if isinstance(details, str) else str(exc)


def _is_rpc_error(exc: Any) -> bool:
    """Report whether *exc* came from a gRPC call, without importing grpc.

    ``grpc.RpcError`` exposes ``code()`` and ``details()``; the rendezvous
    objects raised by streaming calls implement both. Something exposing
    neither did not come from a gRPC call at all.
    """
    return callable(getattr(exc, "code", None)) or callable(
        getattr(exc, "details", None)
    )


def status_code_name(code: Any) -> str:
    """Render a gRPC status as lowercase snake_case, e.g. "not_found".

    Accepts a ``grpc.StatusCode`` (whose ``name`` is the canonical spelling)
    or a bare status name, and reports "unknown" for anything else.
    """
    name = getattr(code, "name", code)
    return name.lower() if isinstance(name, str) else "unknown"


def classify_rpc_error(
    exc: Any,
    *,
    operation: str,
    process_id: int | None = None,
) -> Exception:
    """Translate a sandboxd gRPC error into the SDK's exception taxonomy.

    Pure: callers that need to react to a dead transport (by invalidating the
    channel) keep doing so themselves, since they are the ones holding it.

    Args:
        exc: The ``grpc.RpcError`` raised by the call.
        operation: The RPC name, used in the message (e.g. ``"Execute"``).
        process_id: When set, a ``NOT_FOUND`` status is reported as
            :class:`SandboxdProcessNotFoundError` rather than a generic RPC
            error, since the status then refers to that process.

    Returns:
        The exception to raise, or *exc* itself when it did not come from a
        gRPC call. Never raises on its own account.
    """
    if not _is_rpc_error(exc):
        # Not a gRPC failure, so there is no status to classify. Hand it back
        # rather than dressing a local error up as a sandboxd RPC status.
        return exc if isinstance(exc, Exception) else SandboxError(str(exc))

    code, details = _status(exc)
    # The server's text reaches detail only: it is unbounded and
    # caller-visible data, and a message travels much further than an
    # attribute (logs, tracebacks, span status). An empty one reads as absent
    # rather than as an empty string.
    detail = _truncate_detail(details) or None
    name = status_code_name(code)
    prefix = f"sandboxd {operation} failed ({code})"

    if name == "deadline_exceeded":
        return SandboxTimeoutError(
            f"sandboxd {operation} exceeded its deadline ({code})"
        )
    if name == "unavailable":
        return SandboxConnectionError(
            f"{prefix}: sandboxd is unavailable", "unavailable", detail
        )
    if name == "cancelled":
        # grpcio reports a session the server tore down mid-call this way, and
        # so does our own cancellation -- callers that cancelled deliberately
        # resolve that before asking this function.
        return SandboxConnectionError(
            f"{prefix}: the gRPC session was disconnected", "socket", detail
        )
    if name == "permission_denied":
        return SandboxdPermissionError(prefix, name, detail)
    if name == "not_found" and process_id is not None:
        return SandboxdProcessNotFoundError(
            f"{prefix}: process {process_id} not found", name, detail
        )
    return SandboxdRpcError(prefix, name, detail)
