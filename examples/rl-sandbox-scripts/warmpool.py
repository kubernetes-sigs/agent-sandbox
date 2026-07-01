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

"""Self-contained helpers for managing SandboxTemplate / SandboxWarmPool
resources (Agent Sandbox ``extensions.agents.x-k8s.io/v1beta1``) for SWE-bench
task images.

This is the v1beta1 + current-API re-implementation of the warmpool plumbing
from the rl-tunix forks (``tunix/examples/deepswe/eval_deepswe.py`` and the
``kubernetes-sandbox`` backend in ``R2E-Gym/.../runtime/docker.py``), which
targeted the older ``v1alpha1`` API. See README.md for the differences.
"""

import hashlib
import logging
import time

from kubernetes import client
from kubernetes.stream import stream

logger = logging.getLogger("rl-tunix-swebench.warmpool")

# Agent Sandbox extensions API (current version on GKE / agent-sandbox main).
GROUP = "extensions.agents.x-k8s.io"
VERSION = "v1beta1"
TEMPLATES_PLURAL = "sandboxtemplates"
WARMPOOLS_PLURAL = "sandboxwarmpools"

# A warm sandbox pod must stay alive so it can be claimed and `kubectl exec`'d
# into. SWE-bench images have their own entrypoint, so we override it to idle.
KEEPALIVE_COMMAND = ["sleep", "infinity"]


def get_template_name(docker_image: str) -> str:
  """Maps a Docker image to a unique, DNS-compliant SandboxTemplate name.

  Mirrors ``get_sandbox_template_name`` in the rl-tunix forks so the same image
  always maps to the same template (enabling reuse across tasks).
  """
  img_hash = hashlib.md5(docker_image.encode(),
                         usedforsecurity=False).hexdigest()[:12]
  return f"r2e-img-{img_hash}"


def warmpool_name(template_name: str) -> str:
  return f"pool-{template_name}"


def ensure_template(
    custom_api: client.CustomObjectsApi,
    docker_image: str,
    template_name: str,
    namespace: str,
    *,
    node_selector: dict | None = None,
    image_pull_secret: str | None = None,
    runtime_class: str | None = None,
    cpu: str = "250m",
    memory: str = "512Mi",
) -> None:
  """Creates a SandboxTemplate for ``docker_image`` if it does not yet exist.

  Idempotent: a 404 on GET triggers creation, anything else is re-raised.
  """
  try:
    custom_api.get_namespaced_custom_object(
        group=GROUP,
        version=VERSION,
        namespace=namespace,
        plural=TEMPLATES_PLURAL,
        name=template_name,
    )
    logger.info("SandboxTemplate '%s' already exists.", template_name)
    return
  except client.ApiException as e:
    if e.status != 404:
      raise

  logger.info(
      "Creating SandboxTemplate '%s' for image %s", template_name, docker_image
  )

  pod_spec = {
      "containers": [{
          "name": "agent-runtime",
          "image": docker_image,
          "command": KEEPALIVE_COMMAND,
          "stdin": True,
          "tty": True,
          "resources": {"requests": {"cpu": cpu, "memory": memory}},
      }],
  }
  if node_selector:
    pod_spec["nodeSelector"] = node_selector
  if runtime_class:
    pod_spec["runtimeClassName"] = runtime_class
  if image_pull_secret:
    pod_spec["imagePullSecrets"] = [{"name": image_pull_secret}]

  manifest = {
      "apiVersion": f"{GROUP}/{VERSION}",
      "kind": "SandboxTemplate",
      "metadata": {"name": template_name, "namespace": namespace},
      "spec": {
          "podTemplate": {
              "metadata": {"labels": {"sandbox": template_name}},
              "spec": pod_spec,
          }
      },
  }
  custom_api.create_namespaced_custom_object(
      group=GROUP,
      version=VERSION,
      namespace=namespace,
      plural=TEMPLATES_PLURAL,
      body=manifest,
  )


def create_warmpool(
    custom_api: client.CustomObjectsApi,
    name: str,
    template_name: str,
    replicas: int,
    namespace: str,
) -> None:
  """Creates a SandboxWarmPool of ``replicas`` pre-warmed sandboxes.

  Note the v1beta1 field names (``replicas`` / ``sandboxTemplateRef``); the
  rl-tunix forks used the v1alpha1 names (``size`` / ``templateRef``).
  """
  manifest = {
      "apiVersion": f"{GROUP}/{VERSION}",
      "kind": "SandboxWarmPool",
      "metadata": {"name": name, "namespace": namespace},
      "spec": {
          "replicas": replicas,
          "sandboxTemplateRef": {"name": template_name},
      },
  }
  try:
    custom_api.create_namespaced_custom_object(
        group=GROUP,
        version=VERSION,
        namespace=namespace,
        plural=WARMPOOLS_PLURAL,
        body=manifest,
    )
    logger.info("Created SandboxWarmPool '%s' (replicas=%d)", name, replicas)
  except client.ApiException as e:
    if e.status == 409:
      logger.info("SandboxWarmPool '%s' already exists.", name)
    else:
      raise


def delete_warmpool(
    custom_api: client.CustomObjectsApi, name: str, namespace: str
) -> None:
  """Deletes a SandboxWarmPool (and, transitively, its pre-warmed sandboxes)."""
  try:
    custom_api.delete_namespaced_custom_object(
        group=GROUP,
        version=VERSION,
        namespace=namespace,
        plural=WARMPOOLS_PLURAL,
        name=name,
        body=client.V1DeleteOptions(grace_period_seconds=0),
    )
    logger.info("Deleted SandboxWarmPool '%s'", name)
  except client.ApiException as e:
    if e.status == 404:
      logger.warning("SandboxWarmPool '%s' not found (already deleted).", name)
    else:
      raise


def delete_template(
    custom_api: client.CustomObjectsApi, name: str, namespace: str
) -> None:
  """Deletes a SandboxTemplate. Safe to skip if other pools still need it."""
  try:
    custom_api.delete_namespaced_custom_object(
        group=GROUP,
        version=VERSION,
        namespace=namespace,
        plural=TEMPLATES_PLURAL,
        name=name,
        body=client.V1DeleteOptions(grace_period_seconds=0),
    )
    logger.info("Deleted SandboxTemplate '%s'", name)
  except client.ApiException as e:
    if e.status == 404:
      logger.warning("SandboxTemplate '%s' not found (already deleted).", name)
    else:
      raise


def wait_for_pool_ready(
    custom_api: client.CustomObjectsApi,
    name: str,
    expected: int,
    namespace: str,
    timeout: int = 600,
    poll_interval: int = 5,
) -> bool:
  """Blocks until the warm pool reports ``readyReplicas >= expected``.

  Returns True on success, False if the timeout elapses first. The first wait
  is slow for SWE-bench images (multi-GB pulls) — raise ``timeout`` for those.
  """
  deadline = time.monotonic() + timeout
  while time.monotonic() < deadline:
    obj = custom_api.get_namespaced_custom_object(
        group=GROUP,
        version=VERSION,
        namespace=namespace,
        plural=WARMPOOLS_PLURAL,
        name=name,
    )
    ready = (obj.get("status") or {}).get("readyReplicas", 0)
    logger.info("WarmPool '%s': %d/%d ready", name, ready, expected)
    if ready >= expected:
      return True
    time.sleep(poll_interval)
  logger.error("WarmPool '%s' not ready within %ds", name, timeout)
  return False


def exec_in_pod(
    core_api: client.CoreV1Api,
    pod: str,
    namespace: str,
    command: list[str],
) -> str:
  """Runs ``command`` inside a sandbox pod via the Kubernetes exec API.

  This is the router-free execution path used by the rl-tunix R2E-Gym backend:
  the SDK provisions/claims the sandbox, and commands run through ``kubectl
  exec`` instead of the Sandbox Router HTTP interface. Use this when the
  sandbox-router is not deployed (the SDK's ``sandbox.commands.run`` requires
  it).
  """
  return stream(
      core_api.connect_get_namespaced_pod_exec,
      pod,
      namespace,
      command=command,
      stderr=True,
      stdin=False,
      stdout=True,
      tty=False,
      _preload_content=True,
  )
