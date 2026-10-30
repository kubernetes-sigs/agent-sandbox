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

"""The Harbor environment that runs a task in an agent-sandbox SandboxClaim.

Select it on the Harbor command line by import path::

    harbor run -d terminal-bench@2.0 -a terminus-2 -m <model> \\
        -e k8s_agent_sandbox_harbor:AgentSandboxEnvironment --ek namespace=harbor

Per trial: the task image gets a ``SandboxTemplate`` and a ``SandboxWarmPool``
(once per image per process), a ``SandboxClaim`` is created against the pool
and waited for, and every ``exec`` and file transfer goes through the pod exec
API, so the task image needs nothing installed. ``stop(delete=True)`` deletes
the claim.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import shlex
import time
from pathlib import Path
from typing import Any

from harbor.environments.base import BaseEnvironment, ExecResult
from harbor.environments.capabilities import (EnvironmentCapabilities,
                                              EnvironmentResourceCapabilities)
from harbor.environments.definition import effective_exec_cwd
from harbor.models.trial.config import ResourceMode
from harbor.models.trial.paths import EnvironmentPaths

from . import _clients
from .images import ImageResolver
from .labels import (ANNOTATION_SESSION, LABEL_ENVIRONMENT, LABEL_TRIAL, label_value,
                     object_name)
from .options import EnvironmentOptions
from .templates import CONTAINER, ResourceSpec, common_labels, cpu_quantity
from .transport.archive import pack_dir, pack_file, unpack
from .transport.podexec import PodExecTransport

ENVIRONMENT_TYPE = "agent-sandbox"
_DOCKERFILE = "Dockerfile"
_COMPOSE = "docker-compose.yaml"
_TERMINATE_ATTEMPTS = 3
_PREFLIGHT_TIMEOUT_S = 15


def dockerfile_workdir(text: str) -> str | None:
    """The final ``WORKDIR`` of a Dockerfile, as the image's default cwd.

    Relative WORKDIRs resolve against the previous one, as Docker does. A
    WORKDIR that uses a variable is not resolved and yields None, so the
    image's own default applies.
    """
    workdir: str | None = None
    for raw in text.splitlines():
        line = raw.strip()
        if not line.upper().startswith("WORKDIR"):
            continue
        value = line[len("WORKDIR"):].strip().strip('"').strip("'")
        if not value or "$" in value:
            workdir = None
            continue
        if value.startswith("/") or workdir is None:
            workdir = value
        else:
            workdir = f"{workdir.rstrip('/')}/{value}"
    return workdir


class AgentSandboxEnvironment(BaseEnvironment):
    """A Harbor environment backed by one agent-sandbox claim per trial."""

    def __init__(self, *args: Any, **kwargs: Any):
        # Our --ek keys come in through **kwargs; take them out before the base
        # class sees the rest.
        self._options = EnvironmentOptions.from_kwargs(kwargs)
        # The base constructor calls _validate_definition, which resolves the
        # image, so the resolver exists before super().__init__.
        self._images = ImageResolver(self._options)
        self._image: str | None = None
        self._sandbox: Any = None
        self._transport: PodExecTransport | None = None
        self._pool: str | None = None
        self._timings: dict[str, float] = {}
        super().__init__(*args, **kwargs)
        dockerfile = self.environment_dir / _DOCKERFILE
        self._dockerfile_workdir = (dockerfile_workdir(dockerfile.read_text())
                                    if dockerfile.is_file() else None)
        self.logger: logging.Logger = self.logger.getChild("agent_sandbox")

    # --- Harbor contract: identity and capabilities ----------------------------
    @staticmethod
    def type() -> str:
        return ENVIRONMENT_TYPE

    @property
    def capabilities(self) -> EnvironmentCapabilities:
        # No stream mode (Harbor does not offer it to import-path environments),
        # no accelerators in this version, no network allowlists (a
        # NetworkPolicy cannot express hostnames), no compose, no host mounts.
        return EnvironmentCapabilities()

    @classmethod
    def resource_capabilities(cls) -> EnvironmentResourceCapabilities:
        return EnvironmentResourceCapabilities(
            cpu_limit=True, cpu_request=True, memory_limit=True, memory_request=True,
            fractional_cpus=True)

    @classmethod
    def preflight(cls) -> None:
        """Fail before any trial is queued when the kubeconfig is unusable or
        the cluster does not serve the agent-sandbox extension CRDs.

        Harbor calls this without the ``--ek`` options, so the context comes
        from ``K8S_SANDBOX_HARBOR_KUBE_CONTEXT`` or the kubeconfig's current
        context. An unreachable current context is reported as a warning, not
        a failure, when no context was named: the trials may target another
        one through ``--ek kube_context``, and each trial's ``start()`` fails
        fast with its own error. The API call is bounded, so a context behind
        an unreachable network never hangs the run.
        """
        from kubernetes import client as sync_client
        from kubernetes import config as sync_config

        options = EnvironmentOptions.from_kwargs({})
        named = options.kube_context is not None
        configuration = sync_client.Configuration()
        try:
            try:
                sync_config.load_incluster_config(client_configuration=configuration)
            except sync_config.ConfigException:
                sync_config.load_kube_config(context=options.kube_context,
                                             client_configuration=configuration)
        except Exception as exc:  # noqa: BLE001 - reported to the user as a preflight failure
            raise SystemExit(f"agent-sandbox environment: no usable kubeconfig: {exc}") from exc
        api = sync_client.ApisApi(sync_client.ApiClient(configuration))
        try:
            groups = {g.name for g in api.get_api_versions(_request_timeout=_PREFLIGHT_TIMEOUT_S).groups}
        except Exception as exc:  # noqa: BLE001
            message = (f"agent-sandbox environment: cannot reach the API server of "
                       f"{options.kube_context or 'the current context'} ({configuration.host}): "
                       f"{type(exc).__name__}: {str(exc).splitlines()[0] if str(exc) else ''}")
            if named:
                raise SystemExit(message) from exc
            logging.getLogger(__name__).warning(
                "%s; set K8S_SANDBOX_HARBOR_KUBE_CONTEXT or --ek kube_context to pick the cluster",
                message)
            return
        if "extensions.agents.x-k8s.io" not in groups:
            raise SystemExit("agent-sandbox environment: the cluster does not serve "
                             "extensions.agents.x-k8s.io (install agent-sandbox with "
                             "the extension controllers)")

    def _validate_definition(self) -> None:
        if (self.environment_dir / _COMPOSE).exists():
            raise ValueError(f"{self.type()} does not run docker-compose task environments "
                             f"({self.environment_dir / _COMPOSE})")
        has_dockerfile = (self.environment_dir / _DOCKERFILE).is_file()
        if not self.task_env_config.docker_image and not has_dockerfile:
            raise FileNotFoundError(
                f"task {self.environment_name!r} has neither [environment].docker_image "
                f"nor {self.environment_dir / _DOCKERFILE}")
        # Resolve now, so a task this environment cannot run fails before a
        # claim is created and the error names the task.
        self._image = self._images.resolve(
            docker_image=self.task_env_config.docker_image,
            environment_id=self.environment_id, environment_name=self.environment_name,
            has_dockerfile=has_dockerfile)

    # --- derived values ----------------------------------------------------------
    def _resources(self) -> ResourceSpec:
        """Task resources as Kubernetes quantities, honoring Harbor's enforcement
        policies the way its own Kubernetes provider does: AUTO means request,
        and the limit multipliers derive a limit from the request."""
        cpu_request = self._resource_request_value("cpu", auto_mode=ResourceMode.REQUEST)
        cpu_limit = self._resource_limit_value("cpu", auto_mode=ResourceMode.REQUEST)
        mem_request = self._resource_request_value("memory", auto_mode=ResourceMode.REQUEST)
        mem_limit = self._resource_limit_value("memory", auto_mode=ResourceMode.REQUEST)
        o = self._options
        if cpu_limit is None and cpu_request is not None and o.cpu_limit_multiplier > 0 \
                and self._cpu_resource_mode == ResourceMode.AUTO:
            cpu_limit = cpu_request * o.cpu_limit_multiplier
        if mem_limit is None and mem_request is not None and o.memory_limit_multiplier > 0 \
                and self._memory_resource_mode == ResourceMode.AUTO:
            mem_limit = int(mem_request * o.memory_limit_multiplier)
        storage = self._effective_storage_mb
        return ResourceSpec(
            cpu_request=cpu_quantity(cpu_request) if cpu_request is not None else None,
            cpu_limit=cpu_quantity(cpu_limit) if cpu_limit is not None else None,
            memory_request=f"{int(mem_request)}Mi" if mem_request is not None else None,
            memory_limit=f"{int(mem_limit)}Mi" if mem_limit is not None else None,
            storage_request=f"{int(storage)}Mi" if storage else None,
        )

    @property
    def options(self) -> EnvironmentOptions:
        return self._options

    @property
    def image(self) -> str | None:
        """The image this environment runs, after mirror rewriting."""
        return self._image

    @property
    def claim_name(self) -> str:
        return object_name(f"{self._options.template_prefix}-trial", self.session_id)

    def _claim_labels(self) -> dict[str, str]:
        return {
            **common_labels(self._options),
            LABEL_TRIAL: label_value(self.session_id),
            LABEL_ENVIRONMENT: label_value(self.environment_name),
        }

    def _exec_cwd(self, cwd: str | None) -> str | None:
        return effective_exec_cwd(cwd, self.task_env_config.workdir, self._dockerfile_workdir)

    def _require_transport(self) -> PodExecTransport:
        if self._transport is None:
            raise RuntimeError("environment is not started")
        return self._transport

    # --- lifecycle ---------------------------------------------------------------
    async def start(self, force_build: bool) -> None:
        del force_build  # nothing is built here; the image is prebuilt
        assert self._image is not None
        opts = self._options
        clients = await _clients.get_clients(opts)
        t0 = time.monotonic()
        self._pool = await clients.templates.ensure(
            image=self._image, resources=self._resources(),
            workdir=self.task_env_config.workdir or self._dockerfile_workdir,
            environment_name=self.environment_name, environment_id=self.environment_id)
        self._timings["template_s"] = time.monotonic() - t0

        t1 = time.monotonic()
        self._sandbox = await clients.sandbox_client.create_sandbox(
            self._pool, namespace=opts.namespace,
            sandbox_ready_timeout=opts.ready_timeout_s,
            claim_name=self.claim_name,
            shutdown_after_seconds=opts.claim_ttl_s or None,
            labels=self._claim_labels(),
            pod_annotations={ANNOTATION_SESSION: self.session_id},
        )
        self._timings["claim_ready_s"] = time.monotonic() - t1

        pod = await self._sandbox.get_pod_name()
        self._transport = PodExecTransport(
            api_factory=clients.core_v1_factory, pod=pod, namespace=opts.namespace,
            container=CONTAINER, executor=clients.executor, logger=self.logger,
            max_output_bytes=opts.max_output_bytes)
        t2 = time.monotonic()
        remaining = max(30.0, opts.ready_timeout_s - (time.monotonic() - t1))
        await self._transport.wait_ready(remaining)
        self._timings["exec_ready_s"] = time.monotonic() - t2
        self.logger.info("sandbox %s ready for %s (claim %.1fs, exec %.1fs, pod %s)",
                         self._sandbox.sandbox_id, self.environment_name,
                         self._timings["claim_ready_s"], self._timings["exec_ready_s"], pod)

        paths = EnvironmentPaths.for_os(self.os)
        dirs = [str(p) for p in (paths.agent_dir, paths.verifier_dir, paths.artifacts_dir,
                                 paths.tests_dir, paths.solution_dir)]
        await self.ensure_dirs(self._mount_targets(writable_only=True) + dirs)
        await self._upload_environment_dir_after_start()

    async def stop(self, delete: bool) -> None:
        self._transport = None
        sandbox, self._sandbox = self._sandbox, None
        if sandbox is None:
            return
        if not delete:
            self.logger.info("keeping claim %s in %s; inspect with: kubectl -n %s exec -it "
                             "%s -c %s -- bash", self.claim_name, self._options.namespace,
                             self._options.namespace, getattr(sandbox, "_pod_name", None)
                             or "<pod>", CONTAINER)
            return
        last: Exception | None = None
        for attempt in range(_TERMINATE_ATTEMPTS):
            try:
                # Shielded: a cancelled trial must still release its sandbox.
                await asyncio.shield(sandbox.terminate())
                self.logger.debug("deleted claim %s", self.claim_name)
                return
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - retried, then reported
                last = exc
                await asyncio.sleep(2 * (attempt + 1))
        self.logger.error("could not delete claim %s after %d attempts: %s "
                          "(the claim's shutdown time still bounds it)",
                          self.claim_name, _TERMINATE_ATTEMPTS, last)

    # --- exec and files ----------------------------------------------------------
    async def exec(self, command: str, cwd: str | None = None,
                   env: dict[str, str] | None = None, timeout_sec: int | None = None,
                   user: str | int | None = None) -> ExecResult:
        transport = self._require_transport()
        return await transport.exec(
            command, cwd=self._exec_cwd(cwd), env=self._merge_env(env),
            user=self._resolve_user(user), timeout_sec=timeout_sec)

    async def upload_file(self, source_path: Path | str, target_path: str) -> None:
        source = Path(source_path)
        if not source.is_file():
            raise FileNotFoundError(f"{source} is not a file")
        target_dir, _, name = target_path.rstrip("/").rpartition("/")
        await self._require_transport().upload_tar(pack_file(source, name), target_dir or "/")

    async def upload_dir(self, source_dir: Path | str, target_dir: str) -> None:
        source = Path(source_dir)
        if not source.is_dir():
            self.logger.warning("nothing to upload from %s", source)
            return
        await self._require_transport().upload_tar(pack_dir(source), target_dir)

    async def download_file(self, source_path: str, target_path: Path | str) -> None:
        source_dir, _, name = source_path.rstrip("/").rpartition("/")
        data = await self._require_transport().download_tar(source_dir or "/", single=name)
        target = Path(target_path)
        target.parent.mkdir(parents=True, exist_ok=True)
        import io
        import tarfile
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as tar:
            member = next((m for m in tar.getmembers() if m.isfile()), None)
            if member is None:
                raise FileNotFoundError(f"{source_path} is not a regular file in the sandbox")
            extracted = tar.extractfile(member)
            assert extracted is not None
            target.write_bytes(extracted.read())

    async def download_dir(self, source_dir: str, target_dir: Path | str) -> None:
        data = await self._require_transport().download_tar(source_dir)
        unpack(data, target_dir)

    # --- diagnostics -------------------------------------------------------------
    def timings(self) -> dict[str, float]:
        """Seconds spent in each start step, plus the exec count so far."""
        out = dict(self._timings)
        if self._transport is not None:
            out["exec_count"] = float(self._transport.exec_count)
        return out

    async def write_timings(self, path: str) -> None:
        """Write ``timings()`` as JSON to ``path`` inside the sandbox, so Harbor's
        log download carries it into the trial directory."""
        payload = json.dumps(self.timings(), sort_keys=True)
        with contextlib.suppress(Exception):
            await self.exec(f"mkdir -p {shlex.quote(str(Path(path).parent))} && "
                            f"printf %s {shlex.quote(payload)} > {shlex.quote(path)}",
                            user="root")
