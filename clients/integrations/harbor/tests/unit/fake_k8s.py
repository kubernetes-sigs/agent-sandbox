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

"""Fakes for the Kubernetes and SDK surfaces the environment touches."""

from __future__ import annotations

import io
import subprocess
import tarfile
import tempfile
from pathlib import Path
from typing import Any

from harbor.environments.base import ExecResult


class ApiError(Exception):
    def __init__(self, status: int, reason: str = ""):
        super().__init__(f"{status} {reason}")
        self.status = status
        self.reason = reason


class FakeCustomObjects:
    """Enough of ``CustomObjectsApi`` for the template store: create (409 on
    repeat), list by label selector, delete."""

    def __init__(self) -> None:
        self.objects: dict[tuple[str, str], dict[str, Any]] = {}
        self.calls: list[tuple[str, str, str]] = []

    async def create_namespaced_custom_object(self, *, group, version, namespace, plural, body):
        name = body["metadata"]["name"]
        self.calls.append(("create", plural, name))
        if (plural, name) in self.objects:
            raise ApiError(409, "AlreadyExists")
        self.objects[(plural, name)] = body
        return body

    async def list_namespaced_custom_object(self, *, group, version, namespace, plural,
                                            label_selector="", **_):
        wanted = dict(part.split("=", 1) for part in label_selector.split(",") if part)
        items = [obj for (p, _), obj in self.objects.items() if p == plural
                 and all(obj["metadata"].get("labels", {}).get(k) == v for k, v in wanted.items())]
        return {"items": items}

    async def delete_namespaced_custom_object(self, *, group, version, namespace, plural, name, **_):
        self.calls.append(("delete", plural, name))
        if (plural, name) not in self.objects:
            raise ApiError(404, "NotFound")
        del self.objects[(plural, name)]


class FakeSandbox:
    def __init__(self, sandbox_id: str, pod_name: str, fail_terminate: int = 0):
        self.sandbox_id = sandbox_id
        self._pod_name = pod_name
        self.terminated = 0
        self._fail = fail_terminate

    async def get_pod_name(self) -> str:
        return self._pod_name

    async def terminate(self) -> None:
        if self._fail > 0:
            self._fail -= 1
            raise RuntimeError("transient")
        self.terminated += 1


class FakeSandboxClient:
    def __init__(self) -> None:
        self.created: list[dict[str, Any]] = []
        self.sandboxes: list[FakeSandbox] = []

    async def create_sandbox(self, warmpool, namespace="default", sandbox_ready_timeout=180,
                             labels=None, **kw):
        self.created.append({"warmpool": warmpool, "namespace": namespace,
                             "sandbox_ready_timeout": sandbox_ready_timeout, "labels": labels, **kw})
        sb = FakeSandbox(f"sb-{len(self.created)}", f"sb-{len(self.created)}-pod")
        self.sandboxes.append(sb)
        return sb


class LocalTransport:
    """Runs commands on the local machine inside a scratch root, so the
    environment's exec and tar paths are exercised end to end without a
    cluster. Absolute sandbox paths are mapped under ``root``."""

    def __init__(self) -> None:
        self.root = Path(tempfile.mkdtemp(prefix="fake-sandbox-"))
        self.execs: list[dict[str, Any]] = []
        self.exec_count = 0
        self.ready_waits = 0

    def _map(self, path: str) -> Path:
        return self.root / path.lstrip("/")

    async def wait_ready(self, timeout_s: float) -> None:
        self.ready_waits += 1

    async def exec(self, script, *, cwd=None, env=None, user=None, timeout_sec=None) -> ExecResult:
        self.exec_count += 1
        self.execs.append({"script": script, "cwd": cwd, "env": env, "user": user,
                           "timeout_sec": timeout_sec})
        # Rewrite absolute paths the environment uses into the scratch root so
        # mkdir/test/printf land there; good enough for the lifecycle tests.
        rewritten = script.replace(" /", f" {self.root}/")
        proc = subprocess.run(["bash", "-c", rewritten], capture_output=True, text=True,
                              cwd=self._map(cwd) if cwd and self._map(cwd).is_dir() else None,
                              env={**env} if env else None, timeout=timeout_sec or 60)
        return ExecResult(stdout=proc.stdout, stderr=proc.stderr, return_code=proc.returncode)

    async def upload_tar(self, data: bytes, target_dir: str) -> None:
        target = self._map(target_dir)
        target.mkdir(parents=True, exist_ok=True)
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as tar:
            tar.extractall(target, filter="data")

    async def download_tar(self, source_dir: str, single: str | None = None) -> bytes:
        src = self._map(source_dir)
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as tar:
            if single:
                tar.add(src / single, arcname=single)
            else:
                for child in sorted(src.iterdir()):
                    tar.add(child, arcname=child.name)
        return buf.getvalue()
