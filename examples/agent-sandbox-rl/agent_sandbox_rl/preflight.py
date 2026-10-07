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

"""Per-cluster preflight checks.

Validates each target cluster before provisioning: reachable, the v1beta1
extension CRDs are installed, the controller is up, and (when requested) the
runtime class / image-pull-secret / namespace exist. Hard failures raise
`PreflightError`; soft issues are surfaced as warnings.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field

from kubernetes import client

from . import constants
from .exceptions import PreflightError

logger = logging.getLogger("agent_sandbox_rl.preflight")

# Factory hooks (module-level so tests can patch them).
def _crd_api(cluster):
  return client.ApiextensionsV1Api(cluster.api_client)


def _version_api(cluster):
  return client.VersionApi(cluster.api_client)


def _node_api(cluster):
  return client.NodeV1Api(cluster.api_client)


def _networking_api(cluster):
  return client.NetworkingV1Api(cluster.api_client)


def pod_selector_covers(selector, known_labels: dict) -> bool:
  """Whether a NetworkPolicy ``podSelector`` (a `V1LabelSelector`) is certain to
  select every pod whose labels include ``known_labels``.

  Pods also carry keys outside ``known_labels`` (the per-template ``sandbox``
  label, the controller's own labels, keys other integrations add), so any
  requirement on such a key, positive or negative, cannot be confirmed and counts
  as not covering. An empty selector selects every pod in the namespace."""
  if selector is None:
    return True
  for key, value in (selector.match_labels or {}).items():
    if known_labels.get(key) != value:
      return False
  for req in selector.match_expressions or []:
    if req.key not in known_labels:
      return False
    value = known_labels[req.key]
    values = req.values or []
    if req.operator == "In" and value not in values:
      return False
    if req.operator == "NotIn" and value in values:
      return False
    if req.operator == "DoesNotExist":
      return False
    if req.operator not in ("In", "NotIn", "Exists", "DoesNotExist"):
      return False
  return True


@dataclass
class Check:
  name: str
  ok: bool
  detail: str = ""
  warn_only: bool = False


@dataclass
class PreflightReport:
  cluster: str
  checks: list[Check] = field(default_factory=list)

  def add(self, name, ok, detail="", warn_only=False):
    self.checks.append(Check(name, ok, detail, warn_only))

  @property
  def ok(self) -> bool:
    return all(c.ok for c in self.checks if not c.warn_only)

  @property
  def failures(self) -> list[Check]:
    return [c for c in self.checks if not c.ok and not c.warn_only]

  @property
  def warnings(self) -> list[Check]:
    return [c for c in self.checks if not c.ok and c.warn_only]

  def summary(self) -> str:
    bits = [f"{c.name}={'ok' if c.ok else 'FAIL'}" for c in self.checks]
    return f"[{self.cluster}] " + " ".join(bits)


_EXT_CRDS = (constants.TEMPLATES_PLURAL, constants.WARMPOOLS_PLURAL,
             constants.CLAIMS_PLURAL)


def preflight_cluster(cluster, *, require_runtime_class: str | None = None,
                      image_pull_secret: str | None = None,
                      namespace: str | None = None,
                      validate_template=None,
                      sample_image: str = "busybox:latest",
                      unmanaged_pod_labels: dict | None = None) -> PreflightReport:
  """Run all checks on one cluster and return a `PreflightReport`.

  ``unmanaged_pod_labels`` (the labels every fleet pod carries) is passed when the
  fleet creates its templates with ``networkPolicyManagement: Unmanaged``: the
  controller then creates no NetworkPolicy, so the report warns unless some
  policy in the namespace selects those pods.

  If ``validate_template`` (a `TemplateSpec`) is given, the hand-built
  SandboxTemplate + SandboxWarmPool manifests are server-side dry-run validated
  against the live CRD schema — a clear schema rejection (HTTP 400/422) is a hard
  failure (catches drift the mocked unit tests can't); other errors (RBAC,
  dry-run unsupported) are warnings so we never false-fail.
  """
  r = PreflightReport(cluster.name)
  ns = namespace or cluster.namespace

  # 1. reachability
  try:
    ver = _version_api(cluster).get_code()
    r.add("reachable", True, getattr(ver, "git_version", ""))
  except Exception as e:  # noqa: BLE001
    r.add("reachable", False, str(e))
    return r  # nothing else will work

  # 2. extension CRDs (v1beta1 served) + core Sandbox CRD
  crd_api = _crd_api(cluster)
  wanted = [(p, constants.GROUP, constants.VERSION) for p in _EXT_CRDS]
  wanted.append((constants.SANDBOXES_PLURAL, constants.SANDBOX_GROUP,
                 constants.SANDBOX_VERSION))
  for plural, group, version in wanted:
    name = f"{plural}.{group}"
    try:
      crd = crd_api.read_custom_resource_definition(name)
      served = [v.name for v in crd.spec.versions if v.served]
      r.add(f"crd:{plural}", version in served, ",".join(served) or "none")
    except client.ApiException as e:
      r.add(f"crd:{plural}", False, "not found" if e.status == 404 else str(e))

  # 3. controller up (warning only — claims still work if CRDs exist)
  try:
    dep = cluster.apps_api.read_namespaced_deployment(
        "agent-sandbox-controller", "agent-sandbox-system")
    ready = (dep.status.ready_replicas or 0)
    r.add("controller", ready >= 1, f"{ready} ready", warn_only=True)
  except Exception as e:  # noqa: BLE001
    r.add("controller", False, str(e), warn_only=True)

  # 4. namespace exists (warning — callers may create it)
  try:
    cluster.core_api.read_namespace(ns)
    r.add("namespace", True, ns)
  except client.ApiException as e:
    # Only a successful read is "ok"; a 404 means missing, anything else (403/500)
    # is an access problem we should surface rather than report as passing.
    detail = f"{ns} missing" if e.status == 404 else f"check failed: {e.status}"
    r.add("namespace", False, detail, warn_only=True)

  # 5. runtime class (hard fail if explicitly required)
  if require_runtime_class:
    try:
      _node_api(cluster).read_runtime_class(require_runtime_class)
      r.add(f"runtimeclass:{require_runtime_class}", True)
    except Exception as e:  # noqa: BLE001
      r.add(f"runtimeclass:{require_runtime_class}", False, str(e))

  # 6. image pull secret (hard fail if explicitly required)
  if image_pull_secret:
    try:
      cluster.core_api.read_namespaced_secret(image_pull_secret, ns)
      r.add(f"secret:{image_pull_secret}", True)
    except Exception as e:  # noqa: BLE001
      r.add(f"secret:{image_pull_secret}", False, str(e))

  # 7. CRD manifest schema (server-side dry run) — the hand-built manifests are
  #    the one component with no SDK to lean on; the unit tests only see mocks.
  if validate_template is not None:
    try:
      cluster.resources.validate_manifests(sample_image, validate_template)
      r.add("manifests", True, "dry-run accepted")
    except client.ApiException as e:
      hard = e.status in (400, 422)        # Invalid / BadRequest = real schema drift
      r.add("manifests", False, f"HTTP {e.status}: {e.reason}", warn_only=not hard)
    except Exception as e:  # noqa: BLE001 — connectivity / dry-run unsupported
      r.add("manifests", False, str(e), warn_only=True)

  # 8. Unmanaged templates get no controller-made NetworkPolicy, so without one
  #    of the operator's the fleet's pods are not isolated at all. Warning only:
  #    listing policies may be forbidden for this identity, and the operator may
  #    apply the policy after preflight.
  if unmanaged_pod_labels is not None:
    try:
      policies = _networking_api(cluster).list_namespaced_network_policy(ns).items
      selecting = [p.metadata.name for p in policies
                   if pod_selector_covers(p.spec.pod_selector, unmanaged_pod_labels)]
      if selecting:
        r.add("networkpolicy", True, ", ".join(selecting))
      else:
        wanted = ",".join(f"{k}={v}" for k, v in sorted(unmanaged_pod_labels.items()))
        r.add("networkpolicy", False,
              f"templates are Unmanaged but no NetworkPolicy in {ns} selects the "
              f"fleet's pods ({wanted}); they are not isolated", warn_only=True)
    except client.ApiException as e:
      r.add("networkpolicy", False,
            f"could not list NetworkPolicies in {ns}: HTTP {e.status}", warn_only=True)
    except Exception as e:  # noqa: BLE001 — connectivity
      r.add("networkpolicy", False,
            f"could not list NetworkPolicies in {ns}: {e}", warn_only=True)

  return r


def preflight(registry, *, runtime_class: str | None = None,
              image_pull_secret: str | None = None,
              namespace: str | None = None,
              raise_on_error: bool = True) -> dict:
  """Preflight every cluster in ``registry``.

  Returns ``{cluster_name: PreflightReport}``. Raises `PreflightError` on any
  hard failure unless ``raise_on_error=False``.
  """
  reports = {}
  for c in registry:
    rep = preflight_cluster(
        c, require_runtime_class=runtime_class,
        image_pull_secret=image_pull_secret, namespace=namespace)
    reports[c.name] = rep
    for w in rep.warnings:
      logger.warning("[%s] %s: %s", c.name, w.name, w.detail)
    logger.info(rep.summary())
  if raise_on_error:
    bad = {n: rep.failures for n, rep in reports.items() if not rep.ok}
    if bad:
      detail = "; ".join(
          f"{n}: " + ", ".join(f"{c.name}({c.detail})" for c in fs)
          for n, fs in bad.items())
      raise PreflightError(f"preflight failed — {detail}")
  return reports
