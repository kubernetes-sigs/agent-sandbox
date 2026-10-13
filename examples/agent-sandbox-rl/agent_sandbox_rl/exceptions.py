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

"""Exceptions for agent-sandbox-rl."""


class FleetError(Exception):
  """Base class for all fleet errors."""


class PreflightError(FleetError):
  """A required preflight check failed (cluster/CRDs/capacity)."""


class CapacityError(FleetError):
  """Requested provisioning exceeds available/declared capacity."""


class NoClusterAvailableError(FleetError):
  """Placement could not select a cluster for a task."""


class PoolNotFoundError(FleetError):
  """``adopt_existing`` is on and no pre-existing warm pool serves an image.

  Adopt mode exists so a harness pointed at pools someone else provisioned (the
  multi-cluster fleet layer, a platform team, a previous run) either uses them or
  says so. Without it the miss is silent: `acquire` falls through to the
  on-demand path and builds a parallel size-1 pool per image, which looks like a
  working run and is orders of magnitude slower than the warm pods it ignored."""


class OwnedByAnotherRunError(FleetError):
  """A by-name write would land on an object labelled with another run's id.

  Template and pool names derive from the image, so two runs on the same image
  in one namespace share them. Raised instead of writing to (relabelling,
  building on, resizing) the other run's object."""

  def __init__(self, kind: str, name: str, owner: str):
    super().__init__(f"{kind} '{name}' belongs to run {owner}")
    self.kind = kind
    self.name = name
    self.owner = owner


class FleetOvercommitError(FleetError):
  """The in-SDK circuit breaker tripped: live sandboxes exceeded the safe ceiling
  (``overcommit_factor`` × expected, or ``max_live_sandboxes``), signalling a
  runaway/over-creation. The fleet is torn down before this is raised."""
