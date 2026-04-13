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


class SandboxNotReadyError(SandboxError):
    """Raised when the sandbox is not ready for communication."""


class SandboxNotFoundError(SandboxError):
    """Raised when the sandbox or sandbox claim cannot be found or was deleted."""


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
    ):
        super().__init__(message)
        self.status_code = status_code
        self.response = response
