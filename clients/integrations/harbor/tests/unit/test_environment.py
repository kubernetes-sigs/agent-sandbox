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

"""The environment against Harbor's real base class, with fake clients."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from harbor.environments.base import BaseEnvironment
from harbor.models.task.config import EnvironmentConfig
from harbor.models.trial.config import ResourceMode

from k8s_agent_sandbox_harbor import AgentSandboxEnvironment, ImageUnavailableError
from k8s_agent_sandbox_harbor.labels import LABEL_ENVIRONMENT, LABEL_MANAGED_BY, LABEL_TRIAL
from k8s_agent_sandbox_harbor.templates import POOLS, TEMPLATES

SESSION = "hello-world__bZZeEkw__env"


def make_env(task_dir: Path, trial_paths, env_config, **kwargs) -> AgentSandboxEnvironment:
    return AgentSandboxEnvironment(
        environment_dir=task_dir / "task" / "environment", environment_name="hello-world",
        session_id=SESSION, trial_paths=trial_paths, task_env_config=env_config, **kwargs)


def test_is_a_harbor_environment_with_static_capabilities(task_dir, trial_paths, env_config):
    env = make_env(task_dir, trial_paths, env_config, namespace="harbor")
    assert isinstance(env, BaseEnvironment) and env.type() == "agent-sandbox"
    assert env.capabilities.stream is False and env.capabilities.docker_compose is False
    assert env.resource_capabilities().fractional_cpus is True
    assert env.options.namespace == "harbor"
    assert env.image == "alexgshaw/hello-world:v1"
    assert env.claim_name.startswith("hb-trial-") and len(env.claim_name) <= 63


def test_validate_definition_rejects_compose_and_imageless_tasks(task_dir, trial_paths, env_config):
    env_dir = task_dir / "task" / "environment"
    (env_dir / "docker-compose.yaml").write_text("services: {}")
    with pytest.raises(ValueError, match="docker-compose"):
        make_env(task_dir, trial_paths, env_config)
    (env_dir / "docker-compose.yaml").unlink()
    with pytest.raises(ImageUnavailableError, match="image_map"):
        make_env(task_dir, trial_paths, EnvironmentConfig(cpus=1, memory_mb=1024))
    (env_dir / "Dockerfile").unlink()
    with pytest.raises(FileNotFoundError):
        make_env(task_dir, trial_paths, EnvironmentConfig(cpus=1, memory_mb=1024))


def test_resources_follow_task_and_multipliers(task_dir, trial_paths):
    cfg = EnvironmentConfig(docker_image="i", cpus=0.5, memory_mb=4096, storage_mb=10240)
    r = make_env(task_dir, trial_paths, cfg, memory_limit_multiplier="1.5")._resources()
    assert (r.cpu_request, r.cpu_limit) == ("500m", "500m")
    assert (r.memory_request, r.memory_limit, r.storage_request) == ("4096Mi", "6144Mi", "10240Mi")
    r0 = make_env(task_dir, trial_paths, cfg, cpu_limit_multiplier="0", memory_limit_multiplier="0")._resources()
    assert r0.cpu_limit is None and r0.memory_limit is None
    # Harbor's explicit policies win over the multipliers.
    r_lim = make_env(task_dir, trial_paths, cfg, cpu_enforcement_policy=ResourceMode.LIMIT,
                     memory_enforcement_policy=ResourceMode.IGNORE)._resources()
    assert (r_lim.cpu_request, r_lim.cpu_limit, r_lim.memory_request, r_lim.memory_limit) == \
        (None, "500m", None, None)


async def test_start_exec_files_stop_lifecycle(task_dir, trial_paths, env_config, fake_clients):
    env = make_env(task_dir, trial_paths, env_config, namespace="harbor", warm_replicas="1",
                   claim_ttl_s="7200")
    await env.start(force_build=False)

    # One template + pool for the image, one claim for the trial, with the labels.
    custom, sc, transport = fake_clients["custom"], fake_clients["sandbox_client"], fake_clients["transport"]
    assert {p for p, _ in custom.objects} == {TEMPLATES, POOLS}
    (claim,) = sc.created
    assert claim["warmpool"] == env._pool and claim["namespace"] == "harbor"
    assert claim["shutdown_after_seconds"] == 7200 and claim["claim_name"] == env.claim_name
    assert claim["labels"][LABEL_TRIAL] == SESSION and claim["labels"][LABEL_ENVIRONMENT] == "hello-world"
    assert LABEL_MANAGED_BY in claim["labels"]
    assert transport.ready_waits == 1 and transport.pod == "sb-1-pod"
    # Harbor's standard directories exist (created as root with chmod 777).
    first = transport.execs[0]
    assert first["user"] == "root" and "mkdir -p" in first["script"] and "/logs/agent" in first["script"]
    assert (transport.root / "logs" / "verifier").is_dir() and (transport.root / "tests").is_dir()

    # exec resolves cwd from the Dockerfile WORKDIR and merges persistent env.
    env._persistent_env = {"IS_SANDBOX": "1"}
    r = await env.exec("echo $IS_SANDBOX-$EXTRA", env={"EXTRA": "e"}, timeout_sec=5)
    assert r.return_code == 0 and r.stdout.strip() == "1-e"
    last = transport.execs[-1]
    assert last["cwd"] == "/app" and last["timeout_sec"] == 5 and last["user"] is None
    with env.with_default_user("agent"):
        await env.exec("true")
    assert transport.execs[-1]["user"] == "agent"

    # Files round-trip through the tar paths.
    local = task_dir / "tests"
    (local / "sub").mkdir(parents=True)
    (local / "test.sh").write_text("#!/bin/sh\necho ok\n")
    (local / "sub" / "x").write_text("x")
    await env.upload_dir(local, "/tests")
    assert (transport.root / "tests" / "sub" / "x").read_text() == "x"
    await env.upload_file(local / "test.sh", "/solution/run.sh")
    assert (transport.root / "solution" / "run.sh").read_text().startswith("#!/bin/sh")
    out = task_dir / "down"
    await env.download_dir("/tests", out)
    assert (out / "test.sh").exists() and (out / "sub" / "x").read_text() == "x"
    await env.download_file("/solution/run.sh", task_dir / "run-copy.sh")
    assert (task_dir / "run-copy.sh").read_text().startswith("#!/bin/sh")

    # Timings exist and stop deletes the claim once.
    assert set(env.timings()) >= {"template_s", "claim_ready_s", "exec_ready_s", "exec_count"}
    await env.stop(delete=True)
    assert sc.sandboxes[0].terminated == 1
    await env.stop(delete=True)                     # idempotent
    assert sc.sandboxes[0].terminated == 1
    with pytest.raises(RuntimeError, match="not started"):
        await env.exec("true")


async def test_stop_without_delete_keeps_the_claim(task_dir, trial_paths, env_config, fake_clients):
    env = make_env(task_dir, trial_paths, env_config)
    await env.start(force_build=False)
    await env.stop(delete=False)
    assert fake_clients["sandbox_client"].sandboxes[0].terminated == 0


async def test_stop_retries_a_failed_terminate(task_dir, trial_paths, env_config, fake_clients, monkeypatch):
    import k8s_agent_sandbox_harbor.environment as mod

    async def no_sleep(_):
        return None

    monkeypatch.setattr(mod.asyncio, "sleep", no_sleep)
    env = make_env(task_dir, trial_paths, env_config)
    await env.start(force_build=False)
    sb = fake_clients["sandbox_client"].sandboxes[0]
    sb._fail = 2
    await env.stop(delete=True)
    assert sb.terminated == 1


async def test_prebuilt_task_without_dockerfile_uploads_environment_dir(task_dir, trial_paths,
                                                                        env_config, fake_clients):
    env_dir = task_dir / "task" / "environment"
    (env_dir / "Dockerfile").unlink()
    (env_dir / "data.txt").write_text("seed")
    cfg = EnvironmentConfig(docker_image="alexgshaw/x:1", cpus=1, memory_mb=1024, workdir="/work")
    env = make_env(task_dir, trial_paths, cfg)
    await env.start(force_build=False)
    assert (fake_clients["transport"].root / "work" / "data.txt").read_text() == "seed"


async def test_two_trials_of_one_task_share_the_template(task_dir, trial_paths, env_config, fake_clients):
    a = make_env(task_dir, trial_paths, env_config)
    b = AgentSandboxEnvironment(environment_dir=task_dir / "task" / "environment",
                                environment_name="hello-world", session_id="other__env",
                                trial_paths=trial_paths, task_env_config=env_config)
    await a.start(force_build=False)
    await b.start(force_build=False)
    custom = fake_clients["custom"]
    assert len([c for c in custom.calls if c[0] == "create"]) == 2       # template + pool, once
    assert a.claim_name != b.claim_name and a._pool == b._pool


async def test_write_timings_lands_in_the_sandbox(task_dir, trial_paths, env_config, fake_clients):
    env = make_env(task_dir, trial_paths, env_config)
    await env.start(force_build=False)
    await env.write_timings("/logs/agent-sandbox/timings.json")
    data = json.loads((fake_clients["transport"].root / "logs" / "agent-sandbox" / "timings.json").read_text())
    assert "claim_ready_s" in data


def test_dockerfile_workdir_parsing():
    from k8s_agent_sandbox_harbor.environment import dockerfile_workdir
    assert dockerfile_workdir("FROM x\nWORKDIR /app\n") == "/app"
    assert dockerfile_workdir("FROM x\nWORKDIR /app\nworkdir src\n") == "/app/src"
    assert dockerfile_workdir("FROM x\nWORKDIR $HOME/app\n") is None
    assert dockerfile_workdir("FROM x\nRUN true\n") is None


class _Groups:
    def __init__(self, names):
        self.groups = [type("G", (), {"name": n})() for n in names]


def _patch_kube(monkeypatch, *, groups=None, error=None, incluster=False):
    """Stand in for the kubernetes config loader and the discovery API."""
    from kubernetes import client as sync_client
    from kubernetes import config as sync_config

    calls = {}

    def load_kube_config(context=None, client_configuration=None):
        calls["context"] = context
        client_configuration.host = "https://api.example"

    def load_incluster_config(client_configuration=None):
        if not incluster:
            raise sync_config.ConfigException("not in cluster")

    class FakeApis:
        def __init__(self, api_client):
            pass

        def get_api_versions(self, _request_timeout=None):
            calls["timeout"] = _request_timeout
            if error:
                raise error
            return _Groups(groups or [])

    monkeypatch.setattr(sync_config, "load_kube_config", load_kube_config)
    monkeypatch.setattr(sync_config, "load_incluster_config", load_incluster_config)
    monkeypatch.setattr(sync_client, "ApisApi", FakeApis)
    return calls


def test_preflight_passes_with_the_crds_and_bounds_the_call(monkeypatch):
    calls = _patch_kube(monkeypatch, groups=["apps", "extensions.agents.x-k8s.io"])
    monkeypatch.setenv("K8S_SANDBOX_HARBOR_KUBE_CONTEXT", "prod")
    AgentSandboxEnvironment.preflight()
    assert calls == {"context": "prod", "timeout": 15}


def test_preflight_fails_without_the_crds(monkeypatch):
    _patch_kube(monkeypatch, groups=["apps"])
    with pytest.raises(SystemExit, match="extensions.agents.x-k8s.io"):
        AgentSandboxEnvironment.preflight()


def test_preflight_unreachable_named_context_fails_but_current_context_only_warns(monkeypatch, caplog):
    _patch_kube(monkeypatch, error=TimeoutError("connect timeout"))
    monkeypatch.delenv("K8S_SANDBOX_HARBOR_KUBE_CONTEXT", raising=False)
    with caplog.at_level("WARNING"):
        AgentSandboxEnvironment.preflight()                 # warns, does not exit
    assert "cannot reach the API server of the current context" in caplog.text
    monkeypatch.setenv("K8S_SANDBOX_HARBOR_KUBE_CONTEXT", "prod")
    with pytest.raises(SystemExit, match="cannot reach the API server of prod"):
        AgentSandboxEnvironment.preflight()
