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

"""Shared fixtures: a task directory, Harbor's own config objects, patched clients."""

from __future__ import annotations

import concurrent.futures
from pathlib import Path

import pytest
from harbor.models.task.config import EnvironmentConfig
from harbor.models.trial.paths import TrialPaths

from k8s_agent_sandbox_harbor import _clients
from k8s_agent_sandbox_harbor.options import EnvironmentOptions
from k8s_agent_sandbox_harbor.templates import TemplateStore

from .fake_k8s import FakeCustomObjects, FakeSandboxClient, LocalTransport


@pytest.fixture
def task_dir(tmp_path: Path) -> Path:
    env_dir = tmp_path / "task" / "environment"
    env_dir.mkdir(parents=True)
    (env_dir / "Dockerfile").write_text("FROM python:3.13-slim\nWORKDIR /app\n")
    (tmp_path / "trial").mkdir()
    return tmp_path


@pytest.fixture
def trial_paths(task_dir: Path) -> TrialPaths:
    return TrialPaths(trial_dir=task_dir / "trial")


@pytest.fixture
def env_config() -> EnvironmentConfig:
    return EnvironmentConfig(docker_image="alexgshaw/hello-world:v1", cpus=1, memory_mb=2048,
                             storage_mb=10240)


@pytest.fixture
def fake_clients(monkeypatch: pytest.MonkeyPatch):
    """Patch the shared clients with fakes and return them."""
    custom = FakeCustomObjects()
    sandbox_client = FakeSandboxClient()
    transport = LocalTransport()
    executor = concurrent.futures.ThreadPoolExecutor(max_workers=2)
    state = {"custom": custom, "sandbox_client": sandbox_client, "transport": transport}
    stores: dict[str, TemplateStore] = {}

    async def fake_get_clients(options: EnvironmentOptions):
        # One store per namespace, as the real registry keeps one per process.
        store = stores.get(options.namespace)
        if store is None:
            store = stores[options.namespace] = TemplateStore(custom, options.namespace, options)
        return _clients.Clients(
            sandbox_client=sandbox_client, custom_objects=custom,
            core_v1_factory=lambda: None,  # type: ignore[arg-type,return-value]
            executor=executor, templates=store)

    monkeypatch.setattr(_clients, "get_clients", fake_get_clients)

    # The environment builds a PodExecTransport; swap it for the local one.
    import k8s_agent_sandbox_harbor.environment as env_mod

    def fake_transport(**kwargs):
        transport.pod = kwargs["pod"]
        return transport

    monkeypatch.setattr(env_mod, "PodExecTransport", fake_transport)
    yield state
    executor.shutdown(wait=False)
