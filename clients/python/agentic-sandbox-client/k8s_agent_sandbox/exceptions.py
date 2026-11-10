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

from typing import Any


class SandboxError(RuntimeError):
    """Base class for all sandbox-related errors."""


class SandboxResponseTooLargeError(SandboxError):
    """Raised when a file download exceeds its configured byte limit."""


class SandboxNotReadyError(SandboxError):
    """Raised when the sandbox is not ready for communication."""


class SandboxNoServiceError(SandboxError):
    """Raised in ``in-cluster-service`` mode if the Sandbox has no Service FQDN."""


# Deprecated alias; use SandboxNoServiceError.
SandboxServiceUnavailableError = SandboxNoServiceError


class SandboxNotFoundError(SandboxError):
    """Raised when the sandbox or sandbox claim cannot be found or was deleted."""


class SnapshotNotFoundError(SandboxError):
    """Raised when the requested snapshot does not exist."""


class SandboxTemplateNotFoundError(SandboxError):
    """Raised when the requested sandbox template does not exist."""


class SandboxWarmPoolNotFoundError(SandboxError):
    """Raised when the requested sandbox warm pool does not exist."""


class SandboxPortForwardError(SandboxError):
    """Raised when the port-forward process crashes."""


class SandboxMetadataError(SandboxError):
    """Raised when the sandbox object is missing expected metadata."""


class SandboxRequestError(SandboxError):
    """Raised when an HTTP request to the sandbox fails.

    Attributes:
        status_code: The HTTP status code, if available.
        response: The raw response object (``requests.Response`` or
            ``httpx.Response``), if available.
    """

    def __init__(
        self,
        message: str,
        status_code: int | None = None,
        response: Any = None,
    ) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.response = response


class SandboxClaimFailedError(SandboxError):
    """The SandboxClaim reported a terminal Ready=False reason.

    The claim controller will not retry these (e.g. InvalidMetadata,
    VolumeClaimTemplatesError, ClaimExpired); the claim will not become
    ready without user action, so ready-waits raise instead of waiting
    out the timeout.
    """


class SandboxTimeoutError(SandboxError, TimeoutError):
    """Raised when an operation did not complete within its deadline.

    Also a builtin ``TimeoutError`` so callers written against the previous
    behaviour keep working. That makes it an ``OSError`` too, by way of
    ``TimeoutError``.
    """


class SandboxClosedError(SandboxError):
    """Raised when an operation is attempted on a handle that was closed."""


class SandboxConnectionError(SandboxError):
    """Raised when the sandboxd transport fails outside a timeout or cancellation.

    Attributes:
        kind: Fixed classification of the failure, one of
            ``SANDBOX_CONNECTION_ERROR_KINDS``.
        detail: A truncated diagnostic, or ``None``. Never a raw response
            object, and never the untruncated server text.
    """

    def __init__(
        self,
        message: str,
        kind: str,
        detail: str | None = None,
    ) -> None:
        super().__init__(message)
        self.kind = kind
        self.detail = detail


class SandboxdRpcError(SandboxError):
    """Raised for a sandboxd gRPC status that is not a timeout or a transport failure.

    Attributes:
        code: The gRPC status name in lowercase snake_case, for example
            ``"resource_exhausted"``, or ``"unknown"`` for an unrecognized
            status. Never the raw status details, which go in ``detail``.
        detail: A truncated diagnostic, or ``None``.
    """

    def __init__(
        self,
        message: str,
        code: str,
        detail: str | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.detail = detail


class SandboxdProcessNotFoundError(SandboxdRpcError, LookupError):
    """Raised when sandboxd has no record of the process being addressed.

    Also a builtin ``LookupError``, which is what ``signal()`` raised for a
    ``NOT_FOUND`` status before this class existed.
    """


class SandboxdPermissionError(SandboxdRpcError, PermissionError):
    """Raised when sandboxd refuses an operation on a path or process.

    Also a builtin ``PermissionError``, which is what ``signal()`` raised for a
    ``PERMISSION_DENIED`` status before this class existed. That makes it an
    ``OSError`` too.
    """
