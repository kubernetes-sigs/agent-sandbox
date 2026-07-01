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

"""agent-sandbox-rl: generic, multi-cluster batch orchestration for RL/eval on
Agent Sandbox (v1beta1).

Configure one or many clusters, load tasks, then either drive the primitives
(`SandboxFleet`: preflight → plan → warm pools → acquire → release → teardown)
or the managed runner (`fleet.run(process_fn, strategy, concurrency)`). Async via
`AsyncSandboxFleet`. A `SandboxHandle` (hostname / endpoint / exec) is the
integration point for any RL framework.

See README.md, docs/architecture.md, and docs/design.md.
"""

from . import constants
from .adapters.swebench import SWEBENCH_PROBE, SweBenchSource, swebench_probe
from .cluster import Cluster, ClusterRegistry, build_api_client
from .config import (
    ClusterConfig,
    FleetConfig,
    ObservabilityConfig,
    ResourceSpec,
    TemplateSpec,
)
from .exceptions import (
    CapacityError,
    FleetError,
    NoClusterAvailableError,
    PreflightError,
)
from .async_fleet import AsyncSandboxFleet
from .fleet import FleetPlan, PlanEntry, SandboxFleet
from .handles import SandboxHandle
from .observability import Observer, RunReport, repo_family, serve_metrics
from .placement import (
    CapacityWeighted,
    ImageAffinity,
    LeastLoaded,
    RoundRobin,
    get_placement,
)
from .preflight import PreflightReport, preflight_cluster
from .prepull import prepull, prepull_delete
from .resources import Resources
from .sizing import compute_replicas, plan, recommend_window
from .sources import JsonlSource, ListSource, Task, TaskSource, to_tasks
from .strategies import STRATEGIES, process_parallel

__all__ = [
    "constants",
    # config
    "FleetConfig",
    "ClusterConfig",
    "TemplateSpec",
    "ResourceSpec",
    "ObservabilityConfig",
    # sizing
    "compute_replicas",
    "recommend_window",
    "plan",
    # sources
    "Task",
    "TaskSource",
    "ListSource",
    "JsonlSource",
    "to_tasks",
    # swebench adapter
    "SweBenchSource",
    "swebench_probe",
    "SWEBENCH_PROBE",
    # cluster / resources
    "Resources",
    "Cluster",
    "ClusterRegistry",
    "build_api_client",
    # placement
    "get_placement",
    "RoundRobin",
    "LeastLoaded",
    "CapacityWeighted",
    "ImageAffinity",
    # fleet
    "SandboxFleet",
    "AsyncSandboxFleet",
    "FleetPlan",
    "PlanEntry",
    "SandboxHandle",
    # strategies
    "STRATEGIES",
    "process_parallel",
    # observability
    "Observer",
    "RunReport",
    "repo_family",
    "serve_metrics",
    # preflight / prepull
    "PreflightReport",
    "preflight_cluster",
    "prepull",
    "prepull_delete",
    # exceptions
    "FleetError",
    "PreflightError",
    "CapacityError",
    "NoClusterAvailableError",
]

__version__ = "0.1.0.dev0"


# R2E-Gym adapter helpers are exposed lazily — they need the optional `r2egym`
# extra. Kept out of `__all__` so `import *` never forces the optional import.
def __getattr__(name):
  if name in ("make_fleet_repo_env", "r2egym_command_files"):
    from .adapters import r2egym
    return getattr(r2egym, name)
  raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
