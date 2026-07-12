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

from kubernetes import client, watch

from . import constants
from .config import TemplateSpec

logger = logging.getLogger("agent_sandbox_rl.resources")


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
      self.custom_api.get_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=constants.TEMPLATES_PLURAL,
          name=template_name)
      logger.info("SandboxTemplate '%s' already exists.", template_name)
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
            "name": "agent-runtime",
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
      extra = template.extra_pod_spec
      # Compose the escape hatch with the colocation affinity instead of letting a
      # shallow update() clobber the whole `affinity` key: merge the two affinity
      # blocks (extra_pod_spec wins per sub-key, e.g. its nodeAffinity is added
      # while our podAffinity is preserved unless the user explicitly overrides it).
      if "affinity" in extra and "affinity" in pod_spec:
        merged_affinity = {**pod_spec["affinity"], **extra["affinity"]}
        extra = {**extra, "affinity": merged_affinity}
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
                "metadata": {"labels": {"sandbox": template_name}},
                "spec": pod_spec,
            }
        },
    }

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

  def delete_claim(self, name: str) -> None:
    self._delete(constants.CLAIMS_PLURAL, name, "SandboxClaim")

  def managed_selector(self) -> str:
    return f"{constants.MANAGED_BY_LABEL}={constants.MANAGED_BY_VALUE}"

  def _list(self, plural: str, label_selector: str | None) -> list[str]:
    kwargs = {"label_selector": label_selector} if label_selector else {}
    objs = self.custom_api.list_namespaced_custom_object(
        group=constants.GROUP, version=constants.VERSION,
        namespace=self.namespace, plural=plural, **kwargs)
    return [o["metadata"]["name"] for o in objs.get("items", [])]

  def _delete(self, plural: str, name: str, kind: str) -> None:
    try:
      self.custom_api.delete_namespaced_custom_object(
          group=constants.GROUP, version=constants.VERSION,
          namespace=self.namespace, plural=plural, name=name,
          body=client.V1DeleteOptions(grace_period_seconds=0))
      logger.info("Deleted %s '%s'", kind, name)
    except client.ApiException as e:
      if e.status == 404:
        logger.warning("%s '%s' not found (already deleted).", kind, name)
      else:
        raise
