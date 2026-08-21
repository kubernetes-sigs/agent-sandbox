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

"""Multi-cluster fleet planner + trainer + client-side resolver.

Two ways to consume this package:

* **Operator / planner path** — `fleetctl` CLI + planner internals. Runs out
  of cluster on an operator host or CI/CD.
* **Client path** — :class:`FleetSandboxClient` and :func:`resolve_cluster`
  give consumer code a multi-cluster-aware handle without writing per-cluster
  glue. This is the seam that lets a
  customer write ``client.create_sandbox(template="foo")`` and have the
  fleet layer pick the cluster + swap kubeconfig context under the hood.
"""

# Client-side facade — the public API a workload consumer imports.
from .resolver import (
    AssignmentsMissingError,
    ClusterResolver,
    ClusterUnavailableError,
    FleetSandboxClient,
    ResolvedCluster,
    ResolverError,
    TemplateNotAssignedError,
    gke_context_naming,
    kind_context_naming,
    resolve_cluster,
)

__version__ = "0.0.1"

__all__ = [
    "AssignmentsMissingError",
    "ClusterResolver",
    "ClusterUnavailableError",
    "FleetSandboxClient",
    "ResolvedCluster",
    "ResolverError",
    "TemplateNotAssignedError",
    "__version__",
    "gke_context_naming",
    "kind_context_naming",
    "resolve_cluster",
]
