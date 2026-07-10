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

"""Agent Sandbox API constants (v1beta1 / "beta").

The `k8s-agent-sandbox` SDK ships constants for SandboxClaim and Sandbox, but
NOT for SandboxTemplate / SandboxWarmPool (it never creates them). This package
adds those; they are candidates to upstream into the SDK later.
"""

# Extensions API group (SandboxTemplate / SandboxWarmPool / SandboxClaim).
GROUP = "extensions.agents.x-k8s.io"
VERSION = "v1beta1"
TEMPLATES_PLURAL = "sandboxtemplates"
WARMPOOLS_PLURAL = "sandboxwarmpools"
CLAIMS_PLURAL = "sandboxclaims"

# Core Sandbox API group.
SANDBOX_GROUP = "agents.x-k8s.io"
SANDBOX_VERSION = "v1beta1"
SANDBOXES_PLURAL = "sandboxes"

# Annotations / labels.
POD_NAME_ANNOTATION = "agents.x-k8s.io/pod-name"
SANDBOX_NAME_HASH_LABEL = "agents.x-k8s.io/sandbox-name-hash"

# A warm sandbox pod must stay alive to be claimed and exec'd into. Task images
# have their own entrypoint, so we override it to idle.
KEEPALIVE_COMMAND = ["sleep", "infinity"]

# Default label applied to every resource this package creates (for listing +
# scoped cleanup).
MANAGED_BY_LABEL = "app"
MANAGED_BY_VALUE = "agent-sandbox-rl"
DEFAULT_LABELS = {MANAGED_BY_LABEL: MANAGED_BY_VALUE}
