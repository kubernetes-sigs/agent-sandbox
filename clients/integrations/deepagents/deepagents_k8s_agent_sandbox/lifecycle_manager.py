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

from abc import abstractmethod, ABC
from collections.abc import Callable
import logging

from k8s_agent_sandbox import SandboxClient, SandboxNotFoundError
from k8s_agent_sandbox.sandbox import Sandbox

from .settings import K8sAgentSandboxSettings


logger = logging.getLogger(__name__)


class K8sAgentSandboxLifecycleManager(ABC):
    """A helper class that takes care of managing the sandbox instance."""
    def __init__(self) -> None:
        self._sandbox = None

    def get_sandbox(self) -> Sandbox:
        if self._sandbox is not None:
            return self._sandbox

        self._sandbox = self._get_sandbox()
        return self._sandbox

    @abstractmethod
    def _get_sandbox(self) -> Sandbox:
        pass

class ExistingSandboxInstanceLifecycleManager(K8sAgentSandboxLifecycleManager):
    """Simple manager that uses existing sandbox instance without managing it."""
    def __init__(self, sandbox: Sandbox) -> None:
        super().__init__()
        self._sandbox = sandbox

    def _get_sandbox(self) -> Sandbox:
        return self._sandbox


class ExistingSandboxClaimLifecycleManager(K8sAgentSandboxLifecycleManager):
    """Simple manager that uses existing sandbox by its claim, without managing it."""
    def __init__(
        self,
        client: SandboxClient,
        claim_name: str,
        namespace: str,
    ) -> None:
        super().__init__()
        self._client = client
        self._claim_name = claim_name
        self._namespace = namespace

    def _get_sandbox(self) -> Sandbox:
        return self._client.get_sandbox(
            self._claim_name, 
            namespace=self._namespace
        )


class LabelScopedLifecycleManager(K8sAgentSandboxLifecycleManager):
    """
    Fully manage the lifecycle of a sandbox based on the provided scope.
    Concurrent first runs for the same scope may create duplicate sandboxes, subsequent runs then fail until the duplicates are deleted.
    """
    def __init__(
        self, 
        client: SandboxClient,
        sandbox_settings: K8sAgentSandboxSettings,
        scope: dict[str, str],
        scope_labels_prefix: str,
    ) -> None:
        super().__init__()
        self._client = client
        self._sandbox_settings = sandbox_settings

        self._scope_labels = {
            f"{scope_labels_prefix}/{k}": v for k, v in scope.items()
        }
        
        self._scope_label_selector = ",".join(
            [f"{k}={v}" for k, v in self._scope_labels.items()]
        )

    def _get_sandbox(self) -> Sandbox:
        claim_name = self._find_sandbox_claim_by_label_selector()

        if claim_name is not None:
            try:
                return self._client.get_sandbox(
                    claim_name, 
                    namespace=self._sandbox_settings.namespace
                )
            except SandboxNotFoundError:
                pass

        labels = dict(self._sandbox_settings.labels or {})
        labels.update(self._scope_labels)

        return self._client.create_sandbox(
            self._sandbox_settings.warmpool,
            namespace=self._sandbox_settings.namespace,
            sandbox_ready_timeout=self._sandbox_settings.sandbox_ready_timeout,
            labels=labels,
            shutdown_after_seconds=self._sandbox_settings.shutdown_after_seconds,
            volume_claim_templates=self._sandbox_settings.volume_claim_templates,
            pod_labels=self._sandbox_settings.pod_labels,
            pod_annotations=self._sandbox_settings.pod_annotations,
        )
        
    def _find_sandbox_claim_by_label_selector(self) -> str | None:
    
        found = self._client.list_all_sandboxes(
            namespace=self._sandbox_settings.namespace,
            label_selector=self._scope_label_selector,
        )

        if len(found) > 1:
            raise RuntimeError(
                f"Expected 1 sandbox for scope {self._scope_labels}, found {len(found)}: "
                f"{found}. Delete orphan sandboxes manually."
            )
     
        if len(found) == 1:
            return found[0]
    
        if len(found) == 0:
            return None
