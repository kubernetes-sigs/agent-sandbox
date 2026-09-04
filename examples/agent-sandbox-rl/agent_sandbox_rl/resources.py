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

"""SandboxTemplate / SandboxWarmPool CRUD (v1beta1).

This is the piece the `k8s-agent-sandbox` SDK does not provide. A `Resources`
instance is bound to one cluster's CustomObjectsApi + namespace (so multi-cluster
just means one `Resources` per cluster). Ported and generalized from the
rl-sandbox-scripts example's `warmpool.py`.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass

from kubernetes import client, watch

from . import constants
from .config import TemplateSpec

logger = logging.getLogger("agent_sandbox_rl.resources")


@dataclass(frozen=True)
class DiscoveredPool:
  """A SandboxWarmPool that already exists in the namespace, resolved to the
  image it serves. Produced by `Resources.discover_pools`."""

  pool: str
  template: str
  image: str
  replicas: int


def _deep_merge(base: dict, override: dict) -> dict:
  """Recursively merge override dictionary into base dictionary.

  For list fields where elements are dictionaries with a 'name' key (such as
  container env vars, volume mounts, and ports), elements are merged by name or
  appended. Other list fields and primitive values are replaced by the override.
  """
  merged = dict(base)
  for key, value in override.items():
    if isinstance(value, dict) and isinstance(merged.get(key), dict):
      merged[key] = _deep_merge(merged[key], value)
    elif isinstance(value, list) and isinstance(merged.get(key), list):
      base_list = list(merged[key])
      if all(isinstance(x, dict) and "name" in x for x in base_list) and all(
          isinstance(x, dict) and "name" in x for x in value
      ):
        merged_list = list(base_list)
        for item in value:
          item_name = item.get("name")
          match_idx = next(
              (i for i, x in enumerate(merged_list) if x.get("name") == item_name),
              None,
          )
          if match_idx is not None:
            merged_list[match_idx] = _deep_merge(merged_list[match_idx], item)
          else:
            merged_list.append(dict(item))
        merged[key] = merged_list
      else:
        merged[key] = value
    else:
      merged[key] = value
  return merged


class Resources:
  """Template + warm-pool lifecycle for a single cluster/namespace."""

  def __init__(self, custom_api, core_api, namespace: str,
               *, labels: dict | None = None):
    self.custom_api = custom_api
    self.core_api = core_api
    self.namespace = namespace
    # Always include the management labels (custom labels add to, but cannot
    # drop, them) so teardown's managed_selector still matches everything created.
    self.labels = {**(labels or {}), **constants.DEFAULT_LABELS}

  # --- templates --------------------------------------------------------- #
  def ensure_template(self, image: str, template_name: str,
                      template: TemplateSpec, *, dry_run: bool = False) -> bool:
    """Create the SandboxTemplate for ``image`` if absent. Idempotent.

    Returns True if it created the template, False if it already existed.
    ``dry_run=True`` sends a server-side dry run (``dryRun=All``) — validated
    against the CRD schema but not persisted.
    """
    try:
      existing = self.custom_api.get_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.TEMPLATES_PLURAL,
          name=template_name)
      logger.info("SandboxTemplate '%s' already exists.", template_name)
      # Template names are deterministic per image (r2e-img-<md5>), so a template
      # left over from a previous/other run is reused as-is — and its pod-template
      # labels would carry the OLD run-id, making this run's pods invisible to the
      # circuit breaker and mis-targeted by the reaper (the #1215 safeguards).
      # Reconcile the run/managed labels so pods this run spawns are attributed to
      # this run.
      self._reconcile_template_labels(template_name, existing)
      return False
    except client.ApiException as e:
      if e.status != 404:
        raise

    try:
      self.custom_api.create_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.TEMPLATES_PLURAL,
          body=self._template_manifest(image, template_name, template),
          dry_run="All" if dry_run else None)
    except client.ApiException as e:
      if e.status == 409:        # created concurrently / between our get and create
        logger.info("SandboxTemplate '%s' already exists (409).", template_name)
        return False
      raise
    logger.info("Created SandboxTemplate '%s' for %s", template_name, image)
    return True

  def _template_manifest(self, image: str, template_name: str,
                         template: TemplateSpec) -> dict:
    pod_spec: dict = {
        "containers": [{
            "name": constants.RUNTIME_CONTAINER,
            "image": image,
            "imagePullPolicy": template.image_pull_policy,
            "command": list(template.keepalive_command),
            "stdin": True,
            "tty": True,
            "resources": {"requests": {
                "cpu": template.resources.cpu,
                "memory": template.resources.memory,
            }},
        }],
    }
    if template.runtime_class:
      pod_spec["runtimeClassName"] = template.runtime_class
    if template.node_selector:
      pod_spec["nodeSelector"] = dict(template.node_selector)
    if template.image_pull_secret:
      pod_spec["imagePullSecrets"] = [{"name": template.image_pull_secret}]
    if template.colocate_replicas:
      # Soft: prefer co-locating this pool's replicas (all share the
      # `sandbox=<template>` pod label) on one node so only the first pulls the
      # image and the rest start from the node layer cache. preferred (not
      # required) so it spills instead of dead-locking when a node fills up.
      pod_spec["affinity"] = {
          "podAffinity": {
              "preferredDuringSchedulingIgnoredDuringExecution": [{
                  "weight": 100,
                  "podAffinityTerm": {
                      "labelSelector": {
                          "matchLabels": {"sandbox": template_name}},
                      "topologyKey": "kubernetes.io/hostname",
                  },
              }],
          },
      }
    if template.extra_pod_spec:
      extra = dict(template.extra_pod_spec)
      # Compose the escape hatch with the colocation affinity instead of letting a
      # shallow update() clobber the whole `affinity` key: merge the two affinity
      # blocks (extra_pod_spec wins per sub-key, e.g. its nodeAffinity is added
      # while our podAffinity is preserved unless the user explicitly overrides it).
      if "affinity" in extra and "affinity" in pod_spec:
        merged_affinity = {**pod_spec["affinity"], **extra["affinity"]}
        extra = {**extra, "affinity": merged_affinity}
      if "containers" in extra:
        extra_containers = extra.pop("containers")
        if not isinstance(extra_containers, list):
          raise TypeError(
              f"extra_pod_spec['containers'] must be a list of container dicts, got {type(extra_containers).__name__}"
          )
        if "containers" in pod_spec:
          merged_containers = list(pod_spec["containers"])
          for extra_c in extra_containers:
            if not isinstance(extra_c, dict):
              continue
            c_name = extra_c.get("name")
            if c_name:
              match_idx = next(
                  (i for i, c in enumerate(merged_containers) if c.get("name") == c_name),
                  None,
              )
              if match_idx is not None:
                merged_containers[match_idx] = _deep_merge(merged_containers[match_idx], extra_c)
              else:
                merged_containers.append(dict(extra_c))
            else:
              if merged_containers:
                merged_containers[0] = _deep_merge(merged_containers[0], extra_c)
              else:
                merged_containers.append(dict(extra_c))
          pod_spec["containers"] = merged_containers
      pod_spec.update(extra)

    return {
        "apiVersion": f"{constants.GROUP}/{constants.VERSION}",
        "kind": "SandboxTemplate",
        "metadata": {
            "name": template_name,
            "namespace": self.namespace,
            "labels": dict(self.labels),
        },
        "spec": {
            "podTemplate": {
                # Propagate the fleet labels (incl. the per-run RUN_ID_LABEL) onto
                # the pod template so every sandbox POD carries them — the Sandbox
                # controller does not copy the claim/pool run-id label onto Sandbox
                # CRs, so pods are how a run attributes its live footprint (circuit
                # breaker count + reaper pod sweep). `sandbox=<template>` is kept for
                # the colocation affinity above.
                "metadata": {"labels": {**self.labels, "sandbox": template_name}},
                "spec": pod_spec,
            }
        },
    }

  def _reconcile_template_labels(self, template_name: str, existing: dict) -> None:
    """Patch a pre-existing template's metadata + pod-template labels up to this
    run's labels when they differ, so a reused/leftover template doesn't attribute
    this run's pods to a stale run-id (breaker/reaper correctness, #1215). Only
    patches on mismatch; failures warn (the safeguards degrade, not the run).

    Two concurrent runs sharing an image (same deterministic template name) take
    turns re-labeling this template — pod attribution between their breakers/reapers
    is last-writer-wins. Both directions are safe: a breaker under-counts and fails
    open, and a per-run reap misses the other run's pods rather than deleting them."""
    desired_meta = dict(self.labels)
    desired_pod = {**self.labels, "sandbox": template_name}
    cur_meta = ((existing.get("metadata") or {}).get("labels")) or {}
    cur_pod = ((((existing.get("spec") or {}).get("podTemplate") or {})
                .get("metadata") or {}).get("labels")) or {}
    stale = (any(cur_meta.get(k) != v for k, v in desired_meta.items())
             or any(cur_pod.get(k) != v for k, v in desired_pod.items()))
    if not stale:
      return
    try:
      # patch_namespaced_custom_object sends a JSON Merge Patch (RFC 7386;
      # the client's only content-type for CRD patch is merge-patch+json), which
      # merges nested objects — so this UPSERTS the managed label keys and leaves
      # any pre-existing/operator labels on the template + podTemplate intact. It
      # does not replace the label maps.
      self.custom_api.patch_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.TEMPLATES_PLURAL,
          name=template_name,
          body={"metadata": {"labels": desired_meta},
                "spec": {"podTemplate": {"metadata": {"labels": desired_pod}}}})
      logger.info("Reconciled labels on pre-existing SandboxTemplate '%s' "
                  "(run-id refresh)", template_name)
    except client.ApiException:
      logger.warning("Failed to reconcile labels on SandboxTemplate '%s'; the "
                     "circuit breaker/reaper may under-count this run's pods for "
                     "its image", template_name, exc_info=True)

  def delete_template(self, template_name: str) -> None:
    self._delete(constants.TEMPLATES_PLURAL, template_name, "SandboxTemplate")

  # --- warm pools -------------------------------------------------------- #
  def _warmpool_manifest(self, name: str, template_name: str,
                         replicas: int) -> dict:
    return {
        "apiVersion": f"{constants.GROUP}/{constants.VERSION}",
        "kind": "SandboxWarmPool",
        "metadata": {
            "name": name,
            "namespace": self.namespace,
            "labels": dict(self.labels),
        },
        "spec": {
            "replicas": replicas,
            "sandboxTemplateRef": {"name": template_name},
        },
    }

  def create_warmpool(self, name: str, template_name: str,
                      replicas: int, *, dry_run: bool = False,
                      reconcile: bool = False) -> None:
    """Create a SandboxWarmPool (v1beta1: ``replicas`` + ``sandboxTemplateRef``).

    Idempotent on 409 (already exists). With ``reconcile=True`` a 409 instead
    upserts — patch ``spec.replicas`` to the requested value so a reused/leftover
    pool converges instead of being silently pinned at its old size (which would
    make ``wait_for_pool_ready(expected)`` hang and over-count active replicas).
    Only the warm path needs this; the on-demand claim path leaves it ``False``
    so a hot, repeatedly-reused size-1 pool isn't patched on every claim.
    ``dry_run=True`` sends ``dryRun=All`` and never patches (validation only)."""
    try:
      self.custom_api.create_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.WARMPOOLS_PLURAL,
          body=self._warmpool_manifest(name, template_name, replicas),
          dry_run="All" if dry_run else None)
      logger.info("Created SandboxWarmPool '%s' (replicas=%d)", name, replicas)
    except client.ApiException as e:
      if e.status != 409:
        raise
      if dry_run or not reconcile:
        logger.info("SandboxWarmPool '%s' already exists.", name)
        return
      logger.info("SandboxWarmPool '%s' exists; patching replicas=%d.", name, replicas)
      self.custom_api.patch_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.WARMPOOLS_PLURAL,
          name=name, body={"spec": {"replicas": replicas}})

  def validate_manifests(self, sample_image: str, template: TemplateSpec,
                         *, name: str = "asrl-validate") -> None:
    """Server-side dry-run the hand-built Template + WarmPool manifests against
    the live CRD schema (nothing is persisted).

    These manifests are the one component with no SDK to lean on; every unit test
    stubs the API, so schema drift (a missing required field, wrong nesting) would
    pass the suite and only fail live. Calling this against a real apiserver
    catches that. Propagates ``ApiException`` on rejection; callers decide whether
    to warn or fail.
    """
    self.custom_api.create_namespaced_custom_object(
        group=constants.GROUP, version=constants.VERSION,
        namespace=self.namespace, plural=constants.TEMPLATES_PLURAL,
        body=self._template_manifest(sample_image, name, template),
        dry_run="All")
    self.custom_api.create_namespaced_custom_object(
        group=constants.GROUP, version=constants.VERSION,
        namespace=self.namespace, plural=constants.WARMPOOLS_PLURAL,
        body=self._warmpool_manifest(name, name, 1),
        dry_run="All")

  def delete_warmpool(self, name: str) -> None:
    self._delete(constants.WARMPOOLS_PLURAL, name, "SandboxWarmPool")

  def pool_ready_replicas(self, name: str) -> int:
    obj = self.custom_api.get_namespaced_custom_object(
        group=constants.GROUP, version=constants.VERSION,
        namespace=self.namespace, plural=constants.WARMPOOLS_PLURAL, name=name)
    return int((obj.get("status") or {}).get("readyReplicas", 0) or 0)

  def pool_ready_replicas_safe(self, name: str) -> int:
    """`pool_ready_replicas` that returns 0 instead of raising on API error."""
    try:
      return self.pool_ready_replicas(name)
    except client.ApiException:
      return 0

  def wait_for_pool_ready(self, name: str, expected: int,
                          timeout: int = 600, poll_interval: float = 1.0) -> bool:
    """Block until the pool reports ``readyReplicas >= expected``.

    Uses a Kubernetes **watch** on the WarmPool so readiness is detected at the
    status-update event (near-exact timing — no fixed poll grid). Falls back to a
    short re-check + ``poll_interval`` backoff if the watch drops/reconnects, and
    is bounded by ``timeout``. Returns False on timeout.
    """
    deadline = time.monotonic() + timeout
    # Fast path: already ready (also covers the readiness that landed between
    # pool creation and the watch starting).
    try:
      if self.pool_ready_replicas(name) >= expected:
        return True
    except client.ApiException:
      pass

    w = watch.Watch()
    try:
      while time.monotonic() < deadline:
        remaining = max(1, int(deadline - time.monotonic()))
        try:
          for event in w.stream(
              self.custom_api.list_namespaced_custom_object,
              group=constants.GROUP, version=constants.VERSION,
              namespace=self.namespace, plural=constants.WARMPOOLS_PLURAL,
              field_selector=f"metadata.name={name}",
              timeout_seconds=remaining):
            obj = event.get("object") or {}
            if (obj.get("metadata") or {}).get("name") != name:
              continue                       # bookmarks / belt-and-suspenders
            ready = int((obj.get("status") or {}).get("readyReplicas", 0) or 0)
            logger.info("WarmPool '%s': %d/%d ready", name, ready, expected)
            if ready >= expected:
              return True
          # server closed the watch window; the while-loop re-opens it
        except client.ApiException as e:
          # Terminal errors won't fix themselves — fail fast instead of
          # busy-looping until timeout (e.g. RBAC forbidden, CRD/namespace gone).
          if e.status in (401, 403, 404):
            logger.error("WarmPool '%s' watch failed (HTTP %s); not retrying: %s",
                         name, e.status, e)
            raise
          logger.debug("watch on '%s' interrupted (%s); re-checking", name, e)
          if self.pool_ready_replicas_safe(name) >= expected:
            return True
          time.sleep(poll_interval)
        except Exception as e:  # noqa: BLE001 — connection drop / stale RV
          logger.debug("watch on '%s' dropped (%s); re-checking", name, e)
          if self.pool_ready_replicas_safe(name) >= expected:
            return True
          time.sleep(poll_interval)
    finally:
      w.stop()

    ready = self.pool_ready_replicas_safe(name)
    if ready >= expected:
      return True
    logger.error("WarmPool '%s' not ready (%d/%d) within %ds",
                 name, ready, expected, timeout)
    return False

  # --- listing / helpers ------------------------------------------------- #
  def list_warmpools(self, label_selector: str | None = None) -> list[str]:
    return self._list(constants.WARMPOOLS_PLURAL, label_selector)

  def list_templates(self, label_selector: str | None = None) -> list[str]:
    return self._list(constants.TEMPLATES_PLURAL, label_selector)

  def list_claims(self, label_selector: str | None = None) -> list[str]:
    return self._list(constants.CLAIMS_PLURAL, label_selector)

  def list_sandboxes(self, label_selector: str | None = None) -> list[str]:
    # Sandbox is in the CORE group, not extensions — pass it explicitly.
    return self._list(constants.SANDBOXES_PLURAL, label_selector,
                      group=constants.SANDBOX_GROUP, version=constants.SANDBOX_VERSION)

  def count_pods(self, label_selector: str | None = None) -> int:
    """Count pods matching ``label_selector`` in this namespace (the run's live
    footprint) **without transferring the full pod list** — the circuit breaker
    polls this repeatedly and the target scale is tens of thousands of pods.

    Uses a ``limit=1`` list + ``metadata.remainingItemCount`` (a server-provided
    hint) to get the total cheaply. That hint is only populated for **selector-less**
    lists — the apiserver deliberately returns nil for any label/field predicate
    (``PrepareContinueToken``) — and the breaker always polls with a run-id selector,
    so beyond one page we page through in bounded chunks counting ``len(items)`` only
    (never transferring the whole list in a single response)."""
    kwargs = {"label_selector": label_selector} if label_selector else {}
    resp = self.core_api.list_namespaced_pod(namespace=self.namespace, limit=1, **kwargs)
    meta = resp.metadata
    remaining = getattr(meta, "remaining_item_count", None)
    if remaining is not None:
      return len(resp.items) + remaining     # selector-less fast path
    cont = getattr(meta, "_continue", None)
    if not cont:
      return len(resp.items)                 # single page holds the whole set
    # Selector query with >1 page (no count hint): paginate. Bounds per-request
    # payload + client memory at scale instead of one giant filtered list.
    total = len(resp.items)
    while cont:
      resp = self.core_api.list_namespaced_pod(
          namespace=self.namespace, limit=500, _continue=cont, **kwargs)
      total += len(resp.items)
      cont = getattr(resp.metadata, "_continue", None)
    return total

  def delete_claim(self, name: str) -> None:
    self._delete(constants.CLAIMS_PLURAL, name, "SandboxClaim")

  def delete_sandbox(self, name: str) -> None:
    self._delete(constants.SANDBOXES_PLURAL, name, "Sandbox",
                 group=constants.SANDBOX_GROUP, version=constants.SANDBOX_VERSION)

  def managed_selector(self) -> str:
    return f"{constants.MANAGED_BY_LABEL}={constants.MANAGED_BY_VALUE}"

  # --- adoption ---------------------------------------------------------- #
  def template_images(self, label_selector: str | None = None) -> "dict[str, str]":
    """Map SandboxTemplate name -> the container image it runs.

    Read back off the live objects rather than recomputed from a name, so it
    works for templates this package did not write (no assumption about the
    `<prefix><md5>` scheme, which only holds for our own)."""
    out: dict[str, str] = {}
    for obj in self._list_objects(constants.TEMPLATES_PLURAL, label_selector):
      name = (obj.get("metadata") or {}).get("name")
      containers = ((((obj.get("spec") or {}).get("podTemplate") or {})
                     .get("spec") or {}).get("containers")) or []
      if not name or not containers:
        continue
      chosen = next((c for c in containers
                     if c.get("name") == constants.RUNTIME_CONTAINER), containers[0])
      image = chosen.get("image")
      if image:
        out[name] = image
    return out

  def discover_pools(self, label_selector: str | None = None
                     ) -> "dict[str, DiscoveredPool]":
    """Map image -> `DiscoveredPool` for warm pools already in this namespace.

    Keyed by **image**, not by name, which is the whole point: a pool provisioned
    by something else (the multi-cluster fleet layer names its pools
    ``<template>-pool``, this package names them ``pool-<template>``) is found
    regardless of what it is called. Pools whose template is missing or carries no
    image are skipped — they cannot be matched to a task.

    ``label_selector`` is normally left unset: an adopted pool belongs to its
    provisioner and does not carry our management labels."""
    images = self.template_images(label_selector)
    found: dict[str, DiscoveredPool] = {}
    for obj in self._list_objects(constants.WARMPOOLS_PLURAL, label_selector):
      name = (obj.get("metadata") or {}).get("name")
      spec = obj.get("spec") or {}
      tref = (spec.get("sandboxTemplateRef") or {}).get("name")
      if not name or not tref:
        continue
      image = images.get(tref)
      if image is None:
        logger.debug("warm pool '%s' references template '%s', which was not "
                     "listed (or has no image); skipping", name, tref)
        continue
      replicas = int(spec.get("replicas", 0) or 0)
      prev = found.get(image)
      if prev is None:
        found[image] = DiscoveredPool(name, tref, image, replicas)
        continue
      # Two pools serving one image is legal and happens (a leftover alongside a
      # fresh one). Pick deterministically — deepest wins, name breaks the tie —
      # so repeated planning of the same namespace does not flip between them.
      winner = (DiscoveredPool(name, tref, image, replicas)
                if (replicas, prev.pool) > (prev.replicas, name) else prev)
      logger.warning("image %s is served by more than one warm pool (%s, %s); "
                     "using '%s' (%d replicas)", image, prev.pool, name,
                     winner.pool, winner.replicas)
      found[image] = winner
    return found

  def _list_objects(self, plural: str, label_selector: str | None = None, *,
                    group: str = constants.GROUP,
                    version: str = constants.VERSION) -> list[dict]:
    kwargs = {"label_selector": label_selector} if label_selector else {}
    objs = self.custom_api.list_namespaced_custom_object(
        group=group, version=version,
        namespace=self.namespace, plural=plural, **kwargs)
    return list(objs.get("items", []))

  def _list(self, plural: str, label_selector: str | None, *,
            group: str = constants.GROUP, version: str = constants.VERSION) -> list[str]:
    return [o["metadata"]["name"] for o in
            self._list_objects(plural, label_selector, group=group, version=version)]

  def _delete(self, plural: str, name: str, kind: str, *,
              group: str = constants.GROUP, version: str = constants.VERSION) -> None:
    try:
      self.custom_api.delete_namespaced_custom_object(
          group=group, version=version,
          namespace=self.namespace, plural=plural, name=name,
          body=client.V1DeleteOptions(grace_period_seconds=0))
      logger.info("Deleted %s '%s'", kind, name)
    except client.ApiException as e:
      if e.status == 404:
        logger.warning("%s '%s' not found (already deleted).", kind, name)
      else:
        raise
