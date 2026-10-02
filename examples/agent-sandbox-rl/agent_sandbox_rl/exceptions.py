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


class SandboxStartError(FleetError):
  """A claimed sandbox cannot start and waiting for it cannot help.

  Raised by `SandboxFleet.acquire` when the fail-fast watchdog sees the claim's
  pod in a state the controller will not recover from on its own: an image that
  cannot be pulled, a container that cannot be configured, an OOM-killed
  container, a pod unschedulable past the grace period. Without it the claim sat
  there until ``ready_timeout`` (15 minutes by default). The claim has already
  been deleted when this is raised. ``reason`` is the Kubernetes reason string
  (``ImagePullBackOff``, ``Unschedulable``, ``OOMKilled``, ...)."""

  def __init__(self, reason: str, message: str, *, claim_name: str,
               pod_name: str | None = None, image: str | None = None):
    where = f"pod '{pod_name}'" if pod_name else f"claim '{claim_name}'"
    tail = f" (image {image})" if image else ""
    super().__init__(f"sandbox failed to start: {reason} on {where}{tail}: {message}")
    self.reason = reason
    self.detail = message
    self.claim_name = claim_name
    self.pod_name = pod_name
    self.image = image


class SandboxLostError(FleetError):
  """The sandbox behind a handle is gone (pod deleted, evicted, OOM-killed or
  finished). Raised from ``SandboxHandle.exec`` in place of the raw transport
  error so an RL loop can release the handle and re-acquire at once instead of
  spending its step timeout on a dead pod. ``cause`` is the underlying error."""

  def __init__(self, pod_name: str, claim_name: str, cause: BaseException | None = None):
    super().__init__(
        f"sandbox pod '{pod_name}' (claim '{claim_name}') is gone"
        + (f": {type(cause).__name__}: {cause}" if cause is not None else ""))
    self.pod_name = pod_name
    self.claim_name = claim_name
    self.cause = cause
