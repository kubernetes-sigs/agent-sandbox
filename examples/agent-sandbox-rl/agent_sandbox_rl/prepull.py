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

"""Image pre-pull via a DaemonSet (one init container per image).

Caches task images on every (selected) node before warm pools start, so warm
pods skip the multi-GB pull. Python port of the example's ``prepull.sh``. Runs
per cluster; covers newly-autoscaled nodes automatically.
"""

from __future__ import annotations

import logging
import time

from kubernetes import client

from . import constants

logger = logging.getLogger("agent_sandbox_rl.prepull")

DEFAULT_DS_NAME = "agent-sandbox-rl-prepull"
DEFAULT_PAUSE_IMAGE = "registry.k8s.io/pause:3.10"
_TINY = {"requests": {"cpu": "10m", "memory": "16Mi"}}


def _daemonset_manifest(images, *, ds_name, namespace, node_selector,
                        image_pull_secret, pause_image, labels):
  sel_labels = {**labels, "role": "prepull"}
  init_containers = [
      {"name": f"pull-{i}", "image": img, "imagePullPolicy": "IfNotPresent",
       "command": ["sh", "-c", "exit 0"], "resources": _TINY}
      for i, img in enumerate(images)
  ]
  pod_spec = {
      "terminationGracePeriodSeconds": 0,
      "initContainers": init_containers,
      "containers": [{"name": "pause", "image": pause_image,
                      "imagePullPolicy": "IfNotPresent", "resources": _TINY}],
  }
  if node_selector:
    pod_spec["nodeSelector"] = dict(node_selector)
  if image_pull_secret:
    pod_spec["imagePullSecrets"] = [{"name": image_pull_secret}]
  return {
      "apiVersion": "apps/v1",
      "kind": "DaemonSet",
      "metadata": {"name": ds_name, "namespace": namespace, "labels": dict(labels)},
      "spec": {
          "selector": {"matchLabels": sel_labels},
          "template": {"metadata": {"labels": sel_labels}, "spec": pod_spec},
      },
  }


def prepull(cluster, images, *, node_selector=None, image_pull_secret=None,
            pause_image=DEFAULT_PAUSE_IMAGE, ds_name=DEFAULT_DS_NAME,
            labels=None, wait=True, timeout=1800, poll_interval=4.0) -> bool:
  """Pre-pull ``images`` onto all (selected) nodes of ``cluster``.

  Returns True once every node is ready (or immediately if ``wait=False``).
  """
  labels = dict(labels) if labels else dict(constants.DEFAULT_LABELS)
  uniq = list(dict.fromkeys(images))
  if not uniq:
    return True
  body = _daemonset_manifest(
      uniq, ds_name=ds_name, namespace=cluster.namespace,
      node_selector=node_selector, image_pull_secret=image_pull_secret,
      pause_image=pause_image, labels=labels)
  try:
    cluster.apps_api.create_namespaced_daemon_set(cluster.namespace, body)
    logger.info("Pre-pull DaemonSet '%s' created (%d images)", ds_name, len(uniq))
  except client.ApiException as e:
    if e.status == 409:
      cluster.apps_api.patch_namespaced_daemon_set(ds_name, cluster.namespace, body)
      logger.info("Pre-pull DaemonSet '%s' updated", ds_name)
    else:
      raise
  if not wait:
    return True

  start = time.monotonic()
  deadline = start + timeout
  zero_grace = min(15.0, timeout)   # tolerate "no matching nodes yet" briefly
  while True:
    ds = cluster.apps_api.read_namespaced_daemon_set_status(ds_name, cluster.namespace)
    st = ds.status
    desired = st.desired_number_scheduled or 0
    ready = st.number_ready or 0
    logger.info("Pre-pull '%s': %d/%d nodes ready", ds_name, ready, desired)
    if desired > 0 and ready >= desired:
      return True
    # No nodes match the selector — nothing to pull. Don't hang until timeout.
    if desired == 0 and time.monotonic() - start >= zero_grace:
      logger.warning("Pre-pull '%s': 0 schedulable nodes after %.0fs — nothing "
                     "to pre-pull (check node selector).", ds_name, zero_grace)
      return True
    if time.monotonic() >= deadline:
      logger.error("Pre-pull '%s' incomplete (%d/%d) within %ds",
                   ds_name, ready, desired, timeout)
      return False
    time.sleep(poll_interval)


def prepull_delete(cluster, ds_name=DEFAULT_DS_NAME) -> None:
  """Delete the pre-pull DaemonSet (cached images persist on nodes)."""
  try:
    cluster.apps_api.delete_namespaced_daemon_set(ds_name, cluster.namespace)
    logger.info("Deleted pre-pull DaemonSet '%s'", ds_name)
  except client.ApiException as e:
    if e.status != 404:
      raise
