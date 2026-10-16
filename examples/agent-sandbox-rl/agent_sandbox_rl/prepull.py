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


def _pod_spec(*, node_selector, tolerations, image_pull_secret) -> dict:
  pod_spec: dict = {"terminationGracePeriodSeconds": 0}
  if node_selector:
    pod_spec["nodeSelector"] = dict(node_selector)
  if tolerations:
    pod_spec["tolerations"] = [dict(t) for t in tolerations]
  if image_pull_secret:
    pod_spec["imagePullSecrets"] = [{"name": image_pull_secret}]
  return pod_spec


def _manifest(*, ds_name, namespace, labels, pod_spec) -> dict:
  # The selector carries the DaemonSet's own name so batch DaemonSets that share
  # the managed labels never select each other's pods.
  sel_labels = {**labels, "role": "prepull", "prepull-ds": ds_name}
  return {
      "apiVersion": "apps/v1",
      "kind": "DaemonSet",
      "metadata": {"name": ds_name, "namespace": namespace,
                   "labels": {**labels, "role": "prepull"}},
      "spec": {
          "selector": {"matchLabels": sel_labels},
          "template": {"metadata": {"labels": sel_labels}, "spec": pod_spec},
      },
  }


def _daemonset_manifest(images, *, ds_name, namespace, node_selector,
                        image_pull_secret, pause_image, labels, tolerations=None):
  pod_spec = _pod_spec(node_selector=node_selector, tolerations=tolerations,
                       image_pull_secret=image_pull_secret)
  pod_spec["initContainers"] = [
      {"name": f"pull-{i}", "image": img, "imagePullPolicy": "IfNotPresent",
       "command": ["sh", "-c", "exit 0"], "resources": _TINY}
      for i, img in enumerate(images)
  ]
  pod_spec["containers"] = [{"name": "pause", "image": pause_image,
                             "imagePullPolicy": "IfNotPresent", "resources": _TINY}]
  return _manifest(ds_name=ds_name, namespace=namespace, labels=labels, pod_spec=pod_spec)


def _drop_caches_manifest(*, ds_name, namespace, node_selector, tolerations,
                          pause_image, shell_image, labels):
  pod_spec = _pod_spec(node_selector=node_selector, tolerations=tolerations,
                       image_pull_secret=None)
  pod_spec["initContainers"] = [{
      "name": "drop-caches", "image": shell_image, "imagePullPolicy": "IfNotPresent",
      "command": ["sh", "-c", _DROP_CACHES_CMD],
      "securityContext": {"privileged": True}, "resources": _TINY}]
  pod_spec["containers"] = [{"name": "pause", "image": pause_image,
                             "imagePullPolicy": "IfNotPresent", "resources": _TINY}]
  return _manifest(ds_name=ds_name, namespace=namespace, labels=labels, pod_spec=pod_spec)


def _apply(cluster, body: dict, what: str) -> None:
  name = body["metadata"]["name"]
  try:
    cluster.apps_api.create_namespaced_daemon_set(cluster.namespace, body)
    logger.info("%s DaemonSet '%s' created", what, name)
  except client.ApiException as e:
    if e.status == 409:
      cluster.apps_api.patch_namespaced_daemon_set(name, cluster.namespace, body)
      logger.info("%s DaemonSet '%s' updated", what, name)
    else:
      raise


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
    st = cluster.apps_api.read_namespaced_daemon_set_status(name, cluster.namespace).status
    desired = st.desired_number_scheduled or 0
    ready = st.number_ready or 0
    logger.info("Pre-pull '%s': %d/%d nodes ready", name, ready, desired)
    if desired > 0 and ready >= desired:
      return True
    now = time.monotonic()
    if desired == 0 and now >= zero_grace_until:
      logger.warning("Pre-pull '%s': 0 schedulable nodes — nothing to pre-pull "
                     "(check node selector / tolerations).", name)
      return True
    if now >= deadline:
      logger.error("Pre-pull '%s' incomplete (%d/%d) before the deadline", name, ready, desired)
      return False
    time.sleep(min(poll_interval, max(0.0, deadline - now)) or poll_interval)


def prepull(cluster, images, *, node_selector=None, tolerations=None,
            image_pull_secret=None, pause_image=DEFAULT_PAUSE_IMAGE,
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
  removes each DaemonSet once its nodes are ready (with ``wait`` only), so no
  pre-pull pods remain afterwards; without it the DaemonSets stay and keep
  covering new nodes. ``drop_caches`` then runs a privileged one-shot DaemonSet
  (``<ds_name>-drop-caches``) that drops the page cache on every selected node
  and is deleted afterwards. ``timeout`` is the budget for everything.

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
        tolerations=tolerations)

  if not wait:
    for name, imgs in batches:
      _apply(cluster, body_for(name, imgs), "Pre-pull")
    return True

  start = time.monotonic()
  deadline = start + timeout
  zero_grace_until = start + min(15.0, timeout)
  ok = True
  pending = list(batches)
  running: list[str] = []
  parallel = max(1, int(max_parallel))
  started = 0
  while pending or running:
    # The first batch always starts (a zero budget still records the attempt,
    # as before); further ones only while the budget lasts.
    while pending and len(running) < parallel and (
        started == 0 or time.monotonic() < deadline):
      name, imgs = pending.pop(0)
      _apply(cluster, body_for(name, imgs), "Pre-pull")
      running.append(name)
      started += 1
    if not running:                       # deadline passed before the rest started
      logger.error("Pre-pull: budget of %ds spent with %d DaemonSet(s) not started",
                   timeout, len(pending))
      ok = False
      break
    name = running.pop(0)
    if not _wait_ready(cluster, name, deadline=deadline, poll_interval=poll_interval,
                       zero_grace_until=zero_grace_until):
      ok = False
    if delete_when_done:
      _delete(cluster, name)
  if drop_caches and ok:
    ok = _drop_caches(cluster, ds_name=f"{ds_name}-drop-caches", node_selector=node_selector,
                      tolerations=tolerations, pause_image=pause_image,
                      shell_image=shell_image, labels=labels, deadline=deadline,
                      poll_interval=poll_interval)
  return ok


def _drop_caches(cluster, *, ds_name, node_selector, tolerations, pause_image,
                 shell_image, labels, deadline, poll_interval) -> bool:
  body = _drop_caches_manifest(ds_name=ds_name, namespace=cluster.namespace,
                               node_selector=node_selector, tolerations=tolerations,
                               pause_image=pause_image, shell_image=shell_image,
                               labels=labels)
  _apply(cluster, body, "Drop-caches")
  try:
    return _wait_ready(cluster, ds_name, deadline=deadline, poll_interval=poll_interval,
                       zero_grace_until=min(deadline, time.monotonic() + 15.0))
  finally:
    _delete(cluster, ds_name)


def prepull_delete(cluster, ds_name=DEFAULT_DS_NAME, labels=None) -> None:
  """Delete the pre-pull DaemonSets: the one named ``ds_name`` and every
  DaemonSet in the namespace carrying the managed labels with
  ``role=prepull`` (the batch and drop-caches ones). Cached images persist on
  the nodes."""
  labels = dict(labels) if labels else dict(constants.DEFAULT_LABELS)
  _delete(cluster, ds_name)
  selector = ",".join([*(f"{k}={v}" for k, v in labels.items()), "role=prepull"])
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
