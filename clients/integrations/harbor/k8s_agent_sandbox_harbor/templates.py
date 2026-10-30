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

"""One SandboxTemplate and one SandboxWarmPool per task image.

A Harbor task names an image and its resources. The template renders that as
the pod the controller runs, the pool is the claim target (``replicas: 0``
means every claim cold-starts from the template, ``warm_replicas`` keeps pods
ready). Objects are named by a hash of the rendered spec, so a task whose image
or resources change gets a new pair and an existing pair is never edited.
"""

from __future__ import annotations

import asyncio
import json
import logging
from dataclasses import dataclass
from typing import Any

from .labels import (ANNOTATION_ENVIRONMENT_ID, ANNOTATION_IMAGE, LABEL_ENVIRONMENT,
                     LABEL_IMAGE_HASH, LABEL_MANAGED_BY, MANAGED_BY_VALUE, label_value,
                     object_name, short_hash)
from .options import EnvironmentOptions

GROUP = "extensions.agents.x-k8s.io"
VERSION = "v1beta1"
TEMPLATES = "sandboxtemplates"
POOLS = "sandboxwarmpools"
CLAIMS = "sandboxclaims"
CONTAINER = "main"

# Private ranges the controller's default policy blocks; kept blocked here so
# a task reaches the internet and cluster DNS, and nothing else inside the VPC.
_PRIVATE_V4 = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"]


@dataclass(frozen=True)
class ResourceSpec:
    """Kubernetes quantities for the sandbox container; ``None`` leaves a field out."""
    cpu_request: str | None = None
    cpu_limit: str | None = None
    memory_request: str | None = None
    memory_limit: str | None = None
    storage_request: str | None = None

    def as_resources(self) -> dict[str, dict[str, str]]:
        requests = {k: v for k, v in (("cpu", self.cpu_request), ("memory", self.memory_request),
                                      ("ephemeral-storage", self.storage_request)) if v}
        limits = {k: v for k, v in (("cpu", self.cpu_limit), ("memory", self.memory_limit)) if v}
        out: dict[str, dict[str, str]] = {}
        if requests:
            out["requests"] = requests
        if limits:
            out["limits"] = limits
        return out


def cpu_quantity(cpus: int | float) -> str:
    """Whole cores as ``"2"``, fractions as millicores ``"500m"``."""
    if float(cpus).is_integer():
        return str(int(cpus))
    return f"{int(round(cpus * 1000))}m"


def dns_public_egress() -> list[dict[str, Any]]:
    """Egress to cluster DNS and to public addresses only."""
    return [
        {
            "to": [{
                "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}},
            }],
            "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}],
        },
        {
            "to": [
                {"ipBlock": {"cidr": "0.0.0.0/0", "except": list(_PRIVATE_V4)}},
                {"ipBlock": {"cidr": "::/0"}},
            ],
        },
    ]


def render_pod_spec(image: str, resources: ResourceSpec, options: EnvironmentOptions,
                    workdir: str | None) -> dict[str, Any]:
    container: dict[str, Any] = {
        "name": CONTAINER,
        "image": image,
        # Harbor drives everything through exec; the main process only has to
        # stay alive. Images that define an ENTRYPOINT are overridden on purpose.
        "command": ["sleep", "infinity"],
    }
    if workdir:
        container["workingDir"] = workdir
    res = resources.as_resources()
    if res:
        container["resources"] = res
    if options.run_as_root:
        container["securityContext"] = {"runAsUser": 0, "runAsGroup": 0}
    spec: dict[str, Any] = {
        "automountServiceAccountToken": False,
        "containers": [container],
    }
    if options.run_as_root:
        spec["securityContext"] = {"runAsUser": 0, "runAsGroup": 0}
    if options.runtime_class:
        spec["runtimeClassName"] = options.runtime_class
    if options.node_selector:
        spec["nodeSelector"] = dict(options.node_selector)
    if options.tolerations:
        spec["tolerations"] = [dict(t) for t in options.tolerations]
    if options.image_pull_secret:
        spec["imagePullSecrets"] = [{"name": options.image_pull_secret}]
    return spec


def common_labels(options: EnvironmentOptions) -> dict[str, str]:
    return {LABEL_MANAGED_BY: MANAGED_BY_VALUE, **options.extra_labels}


def render_template(*, name: str, namespace: str, image: str, resources: ResourceSpec,
                    options: EnvironmentOptions, workdir: str | None,
                    environment_name: str, environment_id: str) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "podTemplate": {
            "metadata": {"labels": common_labels(options)},
            "spec": render_pod_spec(image, resources, options, workdir),
        },
    }
    if options.network == "dns-public":
        # The controller's default policy blocks every private range, cluster
        # DNS included, so a task could not resolve a hostname. Harbor tasks
        # install packages and fetch test tooling, thus DNS and public egress.
        # No ingress: the pod exec API reaches the pod through the kubelet.
        spec["networkPolicy"] = {"ingress": [], "egress": dns_public_egress()}
    labels = {
        **common_labels(options),
        LABEL_ENVIRONMENT: label_value(environment_name),
        LABEL_IMAGE_HASH: short_hash(image),
    }
    return {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "SandboxTemplate",
        "metadata": {
            "name": name, "namespace": namespace, "labels": labels,
            "annotations": {ANNOTATION_IMAGE: image, ANNOTATION_ENVIRONMENT_ID: environment_id},
        },
        "spec": spec,
    }


def render_pool(*, name: str, namespace: str, template_name: str, replicas: int,
                labels: dict[str, str]) -> dict[str, Any]:
    return {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "SandboxWarmPool",
        "metadata": {"name": name, "namespace": namespace, "labels": dict(labels)},
        "spec": {"replicas": replicas, "sandboxTemplateRef": {"name": template_name}},
    }


def template_name_for(options: EnvironmentOptions, image: str, resources: ResourceSpec,
                      workdir: str | None) -> str:
    """Name from the rendered pod spec, so equal specs share one template."""
    pod = render_pod_spec(image, resources, options, workdir)
    key = json.dumps({"pod": pod, "network": options.network}, sort_keys=True)
    return object_name(options.template_prefix, key)


def _is_conflict(exc: BaseException) -> bool:
    return getattr(exc, "status", None) == 409


class TemplateStore:
    """Creates the template and pool of an image once per process.

    ``custom_api`` is a ``kubernetes_asyncio`` ``CustomObjectsApi``. A 409 on
    create is success: another trial of the same task got there first, in this
    process or another.
    """

    def __init__(self, custom_api: Any, namespace: str, options: EnvironmentOptions,
                 logger: logging.Logger | None = None):
        self._api = custom_api
        self._namespace = namespace
        self._options = options
        self._logger = logger or logging.getLogger(__name__)
        self._ready: set[str] = set()
        self._locks: dict[str, asyncio.Lock] = {}

    def _lock(self, name: str) -> asyncio.Lock:
        lock = self._locks.get(name)
        if lock is None:
            lock = self._locks[name] = asyncio.Lock()
        return lock

    async def _create(self, plural: str, body: dict[str, Any]) -> bool:
        try:
            await self._api.create_namespaced_custom_object(
                group=GROUP, version=VERSION, namespace=self._namespace, plural=plural, body=body)
            return True
        except Exception as exc:  # noqa: BLE001 - only a 409 is swallowed
            if _is_conflict(exc):
                return False
            raise

    async def ensure(self, *, image: str, resources: ResourceSpec, workdir: str | None,
                     environment_name: str, environment_id: str) -> str:
        """Return the pool name for ``image``, creating template and pool if needed."""
        name = template_name_for(self._options, image, resources, workdir)
        if name in self._ready:
            return name
        async with self._lock(name):
            if name in self._ready:
                return name
            template = render_template(
                name=name, namespace=self._namespace, image=image, resources=resources,
                options=self._options, workdir=workdir, environment_name=environment_name,
                environment_id=environment_id)
            created = await self._create(TEMPLATES, template)
            pool = render_pool(name=name, namespace=self._namespace, template_name=name,
                               replicas=self._options.warm_replicas,
                               labels=template["metadata"]["labels"])
            await self._create(POOLS, pool)
            self._ready.add(name)
            self._logger.info("%s SandboxTemplate/SandboxWarmPool %s for %s (%s)",
                              "Created" if created else "Reusing", name, environment_name, image)
            return name

    async def delete_managed(self, *, plurals: tuple[str, ...] = (CLAIMS, POOLS, TEMPLATES),
                             extra_selector: str = "") -> dict[str, int]:
        """Delete every object this package labelled, claims first. Returns counts."""
        selector = f"{LABEL_MANAGED_BY}={MANAGED_BY_VALUE}"
        if extra_selector:
            selector += f",{extra_selector}"
        counts: dict[str, int] = {}
        for plural in plurals:
            listing = await self._api.list_namespaced_custom_object(
                group=GROUP, version=VERSION, namespace=self._namespace, plural=plural,
                label_selector=selector)
            items = listing.get("items") or []
            for item in items:
                name = item["metadata"]["name"]
                try:
                    await self._api.delete_namespaced_custom_object(
                        group=GROUP, version=VERSION, namespace=self._namespace,
                        plural=plural, name=name)
                except Exception as exc:  # noqa: BLE001 - a vanished object is fine
                    if getattr(exc, "status", None) != 404:
                        raise
            counts[plural] = len(items)
        return counts
