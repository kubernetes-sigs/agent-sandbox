# Copyright 2025 The Kubernetes Authors.
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

from .sandbox_client import SandboxClient
from .sandbox_batch import SandboxBatch
from .metrics_utils import get_metrics, print_metrics
from .models import BatchGroup, Member
from .exceptions import (
    SandboxError,
    SandboxNotFoundError,
    SandboxTemplateNotFoundError,
    SandboxWarmPoolNotFoundError,
    SandboxNotReadyError,
    SandboxClaimFailedError,
    SandboxPortForwardError,
    SandboxRequestError,
    BatchError,
    BatchNotFoundError,
    BatchLeaseExpiredError,
    BatchInUseError,
)


try:
    from .async_sandbox_client import AsyncSandboxClient
except ImportError:
    class AsyncSandboxClient:  # type: ignore[no-redef]
        """Placeholder that raises ImportError when async extras are missing."""
        def __init__(self, *args: object, **kwargs: object) -> None:
            raise ImportError(
                "AsyncSandboxClient requires the 'async' extras. "
                "Install with: pip install k8s-agent-sandbox[async]"
            )


try:
    from .async_sandbox_batch import AsyncSandboxBatch
except ImportError:
    class AsyncSandboxBatch:  # type: ignore[no-redef]
        """Placeholder that raises ImportError when async extras are missing."""
        def __init__(self, *args: object, **kwargs: object) -> None:
            raise ImportError(
                "AsyncSandboxBatch requires the 'async' extras. "
                "Install with: pip install k8s-agent-sandbox[async]"
            )
