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

"""Image pre-pull via DaemonSets, in batches.

Caches task images on every (selected) node before warm pools start, so warm
pods skip the multi-GB pull. Python port of the example's ``prepull.sh``. Runs
per cluster; a DaemonSet left in place covers newly-autoscaled nodes
automatically.

One DaemonSet pod pulls its images as sequential init containers, so a single
DaemonSet with hundreds of images is both an oversized object and a serial
pull. ``images_per_pod`` splits the list into DaemonSets of that many images,
``max_parallel`` bounds how many of them pull at once per node, and
``delete_when_done`` removes each one as its nodes report ready. ``tolerations``
let the pods land on tainted sandbox pools, and ``drop_caches`` finishes with a
privileged one-shot DaemonSet that drops the page cache on every node, for runs
that must start from a cold cache.
"""

from __future__ import annotations

import logging
import time

from kubernetes import client

from . import constants

logger = logging.getLogger("agent_sandbox_rl.prepull")

DEFAULT_DS_NAME = "agent-sandbox-rl-prepull"
DEFAULT_PAUSE_IMAGE = "registry.k8s.io/pause:3.10"
DEFAULT_SHELL_IMAGE = "busybox:1.36"          # drop_caches needs a shell
DEFAULT_IMAGES_PER_POD = 25
DEFAULT_MAX_PARALLEL = 4
_TINY = {"requests": {"cpu": "10m", "memory": "16Mi"}}
_DROP_CACHES_CMD = "sync && echo 3 > /proc/sys/vm/drop_caches"


def _pod_spec(*, node_selector, tolerations, image_pull_secret,
              runtime_class=None) -> dict:
  pod_spec: dict = {"terminationGracePeriodSeconds": 0}
  if runtime_class:
    # A RuntimeClass can carry the nodeSelector and tolerations a pool admits
    # (GKE Sandbox: gvisor); without it the pre-pull pods never schedule there.
    pod_spec["runtimeClassName"] = runtime_class
  if node_selector:
    pod_spec["nodeSelector"] = dict(node_selector)
  if tolerations:
    pod_spec["tolerations"] = [dict(t) for t in tolerations]
  if image_pull_secret:
    pod_spec["imagePullSecrets"] = [{"name": image_pull_secret}]
  return pod_spec


def _selector_labels(ds_name: str) -> dict:
  """``spec.selector`` is immutable, so it carries only run-independent keys:
  the managed-by label, the role and the DaemonSet's own name (batch DaemonSets
  never select each other's pods). The run id lives in the object's and the
  pods' labels, which may change between runs."""
  if len(ds_name) > 63:
    raise ValueError(f"pre-pull DaemonSet name {ds_name!r} is longer than 63 "
                     "characters, the limit for the prepull-ds label value")
  return {**constants.DEFAULT_LABELS, "role": "prepull", "prepull-ds": ds_name}


def _manifest(*, ds_name, namespace, labels, pod_spec) -> dict:
  sel_labels = _selector_labels(ds_name)
  # The object carries the selector labels too: prepull_delete lists DaemonSets
  # by the managed-by label and role, and a caller's `labels` may not include
  # them (FleetConfig.labels replaces the defaults).
  return {
      "apiVersion": "apps/v1",
      "kind": "DaemonSet",
      "metadata": {"name": ds_name, "namespace": namespace,
                   "labels": {**labels, **sel_labels}},
      "spec": {
          "selector": {"matchLabels": sel_labels},
          "template": {"metadata": {"labels": {**labels, **sel_labels}}, "spec": pod_spec},
      },
  }


def _daemonset_manifest(images, *, ds_name, namespace, node_selector,
                        image_pull_secret, pause_image, labels, tolerations=None,
                        runtime_class=None):
  pod_spec = _pod_spec(node_selector=node_selector, tolerations=tolerations,
                       image_pull_secret=image_pull_secret, runtime_class=runtime_class)
  pod_spec["initContainers"] = [
      {"name": f"pull-{i}", "image": img, "imagePullPolicy": "IfNotPresent",
       "command": ["sh", "-c", "exit 0"], "resources": _TINY}
      for i, img in enumerate(images)
  ]
  pod_spec["containers"] = [{"name": "pause", "image": pause_image,
                             "imagePullPolicy": "IfNotPresent", "resources": _TINY}]
  return _manifest(ds_name=ds_name, namespace=namespace, labels=labels, pod_spec=pod_spec)


def _drop_caches_manifest(*, ds_name, namespace, node_selector, tolerations,
                          pause_image, shell_image, labels, image_pull_secret=None):
  # No runtime class here: the pod must reach the host's page cache, which a
  # sandboxed runtime (gVisor) does not expose.
  pod_spec = _pod_spec(node_selector=node_selector, tolerations=tolerations,
                       image_pull_secret=image_pull_secret)
  pod_spec["initContainers"] = [{
      "name": "drop-caches", "image": shell_image, "imagePullPolicy": "IfNotPresent",
      "command": ["sh", "-c", _DROP_CACHES_CMD],
      "securityContext": {"privileged": True}, "resources": _TINY}]
  pod_spec["containers"] = [{"name": "pause", "image": pause_image,
                             "imagePullPolicy": "IfNotPresent", "resources": _TINY}]
  return _manifest(ds_name=ds_name, namespace=namespace, labels=labels, pod_spec=pod_spec)


def _existing_selector(obj) -> dict | None:
  if isinstance(obj, dict):
    return ((obj.get("spec") or {}).get("selector") or {}).get("matchLabels")
  sel = getattr(getattr(obj, "spec", None), "selector", None)
  labels = getattr(sel, "match_labels", None)
  return labels if isinstance(labels, dict) else None


def _apply(cluster, body: dict, what: str, *, deadline: float | None = None) -> None:
  """Create the DaemonSet, or bring an existing one to ``body``. ``deadline``
  (monotonic) bounds the wait for an old object to finish terminating before
  the recreate; past it a ``TimeoutError`` is raised."""
  name = body["metadata"]["name"]
  try:
    cluster.apps_api.create_namespaced_daemon_set(cluster.namespace, body)
    logger.info("%s DaemonSet '%s' created", what, name)
    return
  except client.ApiException as e:
    if e.status != 409:
      raise
  existing = cluster.apps_api.read_namespaced_daemon_set(name, cluster.namespace)
  current = _existing_selector(existing)
  if current is not None and current != body["spec"]["selector"]["matchLabels"]:
    # spec.selector is immutable: a leftover with an older selector (one that
    # carried a run id) can only be recreated.
    logger.info("%s DaemonSet '%s' has another selector; recreating it", what, name)
    _delete(cluster, name)
    for attempt in range(60):
      if deadline is not None and time.monotonic() >= deadline:
        raise TimeoutError(f"{what} DaemonSet '{name}': the old object was still "
                           "terminating when the budget ran out")
      try:
        cluster.apps_api.create_namespaced_daemon_set(cluster.namespace, body)
        logger.info("%s DaemonSet '%s' recreated", what, name)
        return
      except client.ApiException as e:
        if e.status != 409 or attempt == 59:   # still terminating
          raise
        time.sleep(1.0)
  # Replace, not patch: a strategic-merge patch keeps the init containers a
  # shorter image list dropped (merged by name), which then pull unplanned images.
  cluster.apps_api.replace_namespaced_daemon_set(name, cluster.namespace, body)
  logger.info("%s DaemonSet '%s' replaced", what, name)


def _delete(cluster, name: str) -> None:
  try:
    cluster.apps_api.delete_namespaced_daemon_set(name, cluster.namespace)
    logger.info("Deleted DaemonSet '%s'", name)
  except client.ApiException as e:
    if e.status != 404:
      raise


def _wait_ready(cluster, name: str, *, deadline: float, poll_interval: float,
                zero_grace_until: float) -> bool:
  """Poll one DaemonSet until every scheduled node is ready, the deadline
  passes (False), or no node matches its selector past the grace (True)."""
  while True:
    obj = cluster.apps_api.read_namespaced_daemon_set_status(name, cluster.namespace)
    st = obj.status
    desired = st.desired_number_scheduled or 0
    ready = st.number_ready or 0
    # A status the controller has not caught up with (fresh object, or a
    # replaced image list) says 0/0 or still counts the old generation's pods.
    gen = getattr(getattr(obj, "metadata", None), "generation", None)
    seen = getattr(st, "observed_generation", None)
    current = not (isinstance(gen, int) and isinstance(seen, int)) or seen >= gen
    updated = getattr(st, "updated_number_scheduled", None)
    rolled = not isinstance(updated, int) or updated >= desired
    logger.info("Pre-pull '%s': %d/%d nodes ready", name, ready, desired)
    if current and rolled and desired > 0 and ready >= desired:
      return True
    now = time.monotonic()
    if current and desired == 0 and now >= zero_grace_until:
      logger.warning("Pre-pull '%s': 0 schedulable nodes — nothing to pre-pull "
                     "(check node selector / tolerations).", name)
      return True
    if now >= deadline:
      logger.error("Pre-pull '%s' incomplete (%d/%d) before the deadline", name, ready, desired)
      return False
    time.sleep(min(poll_interval, max(0.0, deadline - now)) or poll_interval)


def prepull(cluster, images, *, node_selector=None, tolerations=None,
            runtime_class=None, image_pull_secret=None, pause_image=DEFAULT_PAUSE_IMAGE,
            ds_name=DEFAULT_DS_NAME, labels=None, wait=True, timeout=1800,
            poll_interval=4.0, images_per_pod: int | None = None,
            max_parallel: int = DEFAULT_MAX_PARALLEL, delete_when_done: bool = False,
            drop_caches: bool = False, shell_image=DEFAULT_SHELL_IMAGE) -> bool:
  """Pre-pull ``images`` onto all (selected) nodes of ``cluster``.

  ``images_per_pod=None`` keeps one DaemonSet named ``ds_name`` with every image
  (the original behavior). With a number, the images are split into DaemonSets
  ``<ds_name>-000``, ``-001``, ... of that many images; when ``wait`` is set at
  most ``max_parallel`` of them exist at once (the next starts as the oldest
  finishes), which bounds the concurrent pulls per node. ``delete_when_done``
  removes each DaemonSet once its nodes are ready (a batch that missed the
  deadline stays, so its ``ErrImagePull`` reasons can be read), so no pre-pull
  pods remain afterwards; without it the DaemonSets stay and keep covering new
  nodes. ``drop_caches`` then runs a privileged one-shot DaemonSet
  (``<ds_name>-drop-caches``) that drops the page cache on every selected node
  and is deleted afterwards; it runs with the default runtime (it must reach the
  host), so a pool that admits only a RuntimeClass needs its taint in
  ``tolerations``. The pod is privileged: a namespace under Pod Security
  Admission ``baseline`` or ``restricted`` rejects it, which is reported at once
  (``False``) rather than after the budget. Both need ``wait=True``. ``runtime_class`` goes on the pull
  pods, so they schedule where the sandboxes do. ``timeout`` is the budget for
  everything.

  Returns True once every DaemonSet is ready (or at once when ``wait=False``,
  which creates all of them without pacing), False if the budget ran out.
  """
  labels = dict(labels) if labels else dict(constants.DEFAULT_LABELS)
  uniq = list(dict.fromkeys(images))
  if not uniq:
    return True
  if images_per_pod is None or images_per_pod <= 0 or len(uniq) <= images_per_pod:
    batches = [(ds_name, uniq)]
  else:
    batches = [(f"{ds_name}-{i:03d}", uniq[j:j + images_per_pod])
               for i, j in enumerate(range(0, len(uniq), images_per_pod))]
  logger.info("Pre-pull: %d image(s) in %d DaemonSet(s)", len(uniq), len(batches))

  def body_for(name, imgs):
    return _daemonset_manifest(
        imgs, ds_name=name, namespace=cluster.namespace, node_selector=node_selector,
        image_pull_secret=image_pull_secret, pause_image=pause_image, labels=labels,
        tolerations=tolerations, runtime_class=runtime_class)

  if not wait:
    if delete_when_done or drop_caches:
      logger.warning("prepull(wait=False): delete_when_done / drop_caches need "
                     "wait=True and are skipped")
    for name, imgs in batches:
      _apply(cluster, body_for(name, imgs), "Pre-pull")
    return True

  start = time.monotonic()
  deadline = start + timeout
  ok = True
  pending = list(batches)
  running: list[tuple[str, float]] = []       # (name, created at)
  parallel = max(1, int(max_parallel))
  started = 0
  while pending or running:
    # The first batch always starts (a zero budget still records the attempt,
    # as before); further ones only while the budget lasts.
    while pending and len(running) < parallel and (
        started == 0 or time.monotonic() < deadline):
      name, imgs = pending.pop(0)
      try:
        _apply(cluster, body_for(name, imgs), "Pre-pull", deadline=deadline)
      except TimeoutError as exc:
        logger.error("Pre-pull: %s; %d DaemonSet(s) not started", exc, len(pending) + 1)
        ok = False
        pending.clear()
        break
      running.append((name, time.monotonic()))
      started += 1
    if not running:                       # deadline passed before the rest started
      if pending:
        logger.error("Pre-pull: budget of %ds spent with %d DaemonSet(s) not started",
                     timeout, len(pending))
      ok = False
      break
    name, created_at = running.pop(0)
    # The 0/0 grace is per DaemonSet, from its creation: a fresh object reports
    # desired=0 until the controller reconciles it, however long the batches
    # before it took.
    batch_ok = _wait_ready(cluster, name, deadline=deadline, poll_interval=poll_interval,
                           zero_grace_until=min(deadline, created_at + 15.0))
    if not batch_ok:
      ok = False
    if delete_when_done and batch_ok:
      _delete(cluster, name)
  if drop_caches and ok:
    ok = _drop_caches(cluster, ds_name=f"{ds_name}-drop-caches", node_selector=node_selector,
                      tolerations=tolerations, pause_image=pause_image,
                      shell_image=shell_image, labels=labels, deadline=deadline,
                      poll_interval=poll_interval, image_pull_secret=image_pull_secret)
  return ok


def _drop_caches(cluster, *, ds_name, node_selector, tolerations, pause_image,
                 shell_image, labels, deadline, poll_interval,
                 image_pull_secret=None) -> bool:
  body = _drop_caches_manifest(ds_name=ds_name, namespace=cluster.namespace,
                               node_selector=node_selector, tolerations=tolerations,
                               pause_image=pause_image, shell_image=shell_image,
                               labels=labels, image_pull_secret=image_pull_secret)
  try:
    _apply(cluster, body, "Drop-caches", deadline=deadline)
  except TimeoutError as exc:
    logger.error("Drop-caches: %s", exc)
    return False
  except client.ApiException as e:
    if e.status == 403:
      # Pod Security Admission at baseline or restricted rejects the privileged
      # pod outright; say so now instead of polling the budget away.
      logger.error("Drop-caches: the namespace rejects privileged pods (403; Pod "
                   "Security Admission baseline/restricted?). Label the namespace "
                   "pod-security.kubernetes.io/enforce=privileged or set "
                   "drop_caches=False: %s", e.reason)
      return False
    raise
  try:
    return _wait_ready(cluster, ds_name, deadline=deadline, poll_interval=poll_interval,
                       zero_grace_until=min(deadline, time.monotonic() + 15.0))
  finally:
    _delete(cluster, ds_name)


def prepull_delete(cluster, ds_name=DEFAULT_DS_NAME, labels=None) -> None:
  """Delete the pre-pull DaemonSets: the one named ``ds_name`` and every
  DaemonSet in the namespace that this package created with ``role=prepull``
  (the batch and drop-caches ones), of any run: the selection uses the
  managed-by label only, so an earlier run's leftovers go too (``labels`` is
  accepted for compatibility and not used to narrow it). Cached images
  persist on the nodes."""
  del labels
  _delete(cluster, ds_name)
  selector = ",".join([*(f"{k}={v}" for k, v in constants.DEFAULT_LABELS.items()),
                       "role=prepull"])
  try:
    found = cluster.apps_api.list_namespaced_daemon_set(
        cluster.namespace, label_selector=selector).items
  except client.ApiException as e:
    logger.warning("prepull_delete: could not list DaemonSets (%s): %s", selector, e)
    return
  for ds in found or []:
    name = getattr(getattr(ds, "metadata", None), "name", None)
    if name and name != ds_name:
      _delete(cluster, name)
