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

"""R2E-Gym adapter tests — run with NO cluster and NO real r2egym.

Layers: (1) the lazy guard raises cleanly when r2egym is absent; (2) the override
logic (warm-pod binding, no-op teardown, **namespace forwarding incl. under
concurrency**, no-cold-start, bind-failure, reset) is unit-tested by injecting a
fake r2egym base into sys.modules and a fake kubernetes `stream`.
"""

import sys
import threading
import types
from unittest.mock import MagicMock

import pytest

from agent_sandbox_rl.adapters import r2egym as adapter
from agent_sandbox_rl.handles import SandboxHandle
from agent_sandbox_rl.sources import Task


def _reset_classes():
  adapter._CLASSES = None


# --- layer 1: guard with r2egym absent ------------------------------------ #
def test_guard_without_r2egym(monkeypatch):
  _reset_classes()
  monkeypatch.setitem(sys.modules, "r2egym", None)   # force import failure
  with pytest.raises(RuntimeError, match="R2E-Gym"):
    adapter._build_classes()
  with pytest.raises(RuntimeError, match="R2E-Gym"):
    _ = adapter.FleetRepoEnv  # PEP 562 lazy access
  _reset_classes()


# --- fakes for layer 2 ---------------------------------------------------- #
class _FakeResp:
  """Minimal kubernetes stream() response. stdout echoes the namespace it was
  given, so callers can assert the exec targeted the right namespace."""

  def __init__(self, namespace):
    self._out = namespace
    self._open = True
    self.returncode = 0

  def is_open(self):
    if self._open:
      self._open = False
      return True
    return False

  def update(self, timeout=None):
    pass

  def peek_stdout(self):
    return bool(self._out)

  def read_stdout(self):
    o, self._out = self._out, ""
    return o

  def peek_stderr(self):
    return False

  def read_stderr(self):
    return ""

  def write_stdin(self, data):
    pass

  def close(self):
    pass


class _FakeBaseRuntime:
  """Stand-in for r2egym DockerRuntime: dispatches lifecycle like the real one,
  including the base's swallow of _start_kubernetes_sandbox exceptions."""

  def __init__(self, ds, docker_image=None, command=None, logger=None,
               backend="docker", **kw):
    self.ds = ds
    self.docker_image = docker_image
    self.command = command
    self.logger = logger or MagicMock()
    self.backend = backend
    self.repo_name = "repo"
    self.docker_kwargs = kw
    self.container = None
    self.container_name = None
    self.start_container(docker_image, command, None)   # → _start_kubernetes_sandbox
    self.setup_env()

  def start_container(self, image, command, name, **kw):
    if self.backend == "kubernetes-sandbox":
      try:                                  # mirror base docker.py swallow
        self._start_kubernetes_sandbox()
      except Exception:  # noqa: BLE001
        self.stop_container()
        return

  def stop_container(self):
    if self.backend == "kubernetes-sandbox":
      self._stop_kubernetes_sandbox()

  def close(self):
    self.stop_container()

  def setup_env(self):
    pass

  def add_commands(self, files):
    for f in files:
      self._copy_to_container_kubernetes(f, "/usr/local/bin/x")

  def get_task_instruction(self):
    return "instruction"

  # overridden by the adapter subclass:
  def _start_kubernetes_sandbox(self):
    raise NotImplementedError

  def _stop_kubernetes_sandbox(self):
    raise NotImplementedError


class _FakeBaseRepoEnv:
  def add_commands(self, files):
    self.runtime.add_commands(files)


class _FakeEnvArgs:
  def __init__(self, ds=None, repo_path=None, docker_image=None):
    self.ds = ds


class _FakeParseCommandBash:
  pass


def _install_fake_r2egym(monkeypatch):
  def mod(name):
    m = types.ModuleType(name)
    monkeypatch.setitem(sys.modules, name, m)
    return m

  mod("r2egym")
  mod("r2egym.agenthub")
  mod("r2egym.agenthub.environment")
  env_mod = mod("r2egym.agenthub.environment.env")
  env_mod.EnvArgs = _FakeEnvArgs
  env_mod.RepoEnv = _FakeBaseRepoEnv
  mod("r2egym.agenthub.runtime")
  docker_mod = mod("r2egym.agenthub.runtime.docker")
  docker_mod.DockerRuntime = _FakeBaseRuntime
  docker_mod.DEFAULT_NAMESPACE = "default"
  docker_mod.CMD_TIMEOUT = 900
  mod("r2egym.agenthub.utils")
  log_sub = mod("r2egym.agenthub.utils.log")
  log_sub.get_logger = lambda *a, **k: MagicMock()
  mod("r2egym.agenthub.agent")
  cmd_mod = mod("r2egym.agenthub.agent.commands")
  cmd_mod.ParseCommandBash = _FakeParseCommandBash
  return docker_mod


@pytest.fixture
def fake_r2egym(monkeypatch):
  _reset_classes()
  docker_mod = _install_fake_r2egym(monkeypatch)

  fake_pod = object()
  fake_core = MagicMock()
  fake_core.read_namespaced_pod.return_value = fake_pod
  monkeypatch.setattr("kubernetes.client.ApiClient", lambda cfg: MagicMock())
  monkeypatch.setattr("kubernetes.client.CoreV1Api", lambda api=None: fake_core)

  copied_ns = []   # namespaces seen by file-copy

  def fake_stream(func, name, namespace, **kw):
    if kw.get("stdin"):              # the file-copy path
      copied_ns.append(namespace)
    return _FakeResp(namespace)

  monkeypatch.setattr("agent_sandbox_rl.adapters.r2egym.stream", fake_stream)

  yield types.SimpleNamespace(docker_mod=docker_mod, core=fake_core,
                              pod=fake_pod, copied_ns=copied_ns)
  _reset_classes()


def _handle(ns="rl-ns", pod="pod-1", with_ds=True):
  cluster = types.SimpleNamespace(
      name="c1", namespace=ns,
      api_client=types.SimpleNamespace(configuration=object()),
      core_api=MagicMock())
  meta = {"repo": "django/django"}
  if with_ds:
    meta["ds"] = {"instance_id": "x", "docker_image": "img", "repo": "django/django"}
  task = Task(id="x", image="img:latest", metadata=meta)
  return SandboxHandle(task=task, cluster_name="c1", claim_name="claim-1",
                       sandbox_id="sb-1", pod_name=pod, hostname="sb-1",
                       pod_ip="10.0.0.1", sandbox=None, _cluster=cluster)


# --- layer 2: override logic ---------------------------------------------- #
def test_start_binds_warm_pod(fake_r2egym):
  FleetDockerRuntime, _ = adapter._build_classes()
  rt = FleetDockerRuntime(_handle(ns="rl-ns"))
  assert rt.container_name == "pod-1"
  assert rt.sb_client is None and rt.custom_api is None
  assert rt.container is fake_r2egym.pod
  _, kwargs = fake_r2egym.core.read_namespaced_pod.call_args
  assert kwargs["name"] == "pod-1" and kwargs["namespace"] == "rl-ns"


def test_stop_is_noop(fake_r2egym):
  FleetDockerRuntime, _ = adapter._build_classes()
  rt = FleetDockerRuntime(_handle())
  rt._stop_kubernetes_sandbox()
  rt.stop_container()
  assert not fake_r2egym.core.delete_namespaced_pod.called


def test_run_uses_handle_namespace(fake_r2egym):
  FleetDockerRuntime, _ = adapter._build_classes()
  rt = FleetDockerRuntime(_handle(ns="rl-ns"))
  out, code = rt._run_kubernetes("echo hi", workdir="/testbed")
  assert out == "rl-ns"        # fake echoes the namespace it execed against
  assert code == "0"


def test_copy_uses_handle_namespace(fake_r2egym, tmp_path):
  FleetDockerRuntime, _ = adapter._build_classes()
  rt = FleetDockerRuntime(_handle(ns="rl-ns"))
  src = tmp_path / "x.py"
  src.write_text("x")
  rt._copy_to_container_kubernetes(str(src), "/dst/x.py")
  assert fake_r2egym.copied_ns == ["rl-ns"]


def test_namespace_isolated_under_concurrency(fake_r2egym):
  # Two runtimes on different namespaces, exec'ing concurrently: each call must
  # target its OWN namespace (regression guard for the old global-mutation bug).
  FleetDockerRuntime, _ = adapter._build_classes()
  rt_a = FleetDockerRuntime(_handle(ns="ns-a", pod="pod-a"))
  rt_b = FleetDockerRuntime(_handle(ns="ns-b", pod="pod-b"))
  errors = []
  barrier = threading.Barrier(2)

  def worker(rt, expected):
    barrier.wait()
    for _ in range(50):
      out, _code = rt._run_kubernetes("echo")
      if out != expected:
        errors.append((expected, out))

  ta = threading.Thread(target=worker, args=(rt_a, "ns-a"))
  tb = threading.Thread(target=worker, args=(rt_b, "ns-b"))
  ta.start()
  tb.start()
  ta.join()
  tb.join()
  assert errors == []


def test_bind_failure_raises(fake_r2egym):
  fake_r2egym.core.read_namespaced_pod.side_effect = RuntimeError("pod gone")
  FleetDockerRuntime, _ = adapter._build_classes()
  with pytest.raises(RuntimeError, match="failed to bind warm pod"):
    FleetDockerRuntime(_handle())


def test_reset_rebinds_without_cold_start(fake_r2egym):
  _, FleetRepoEnv = adapter._build_classes()
  from r2egym.agenthub.environment.env import EnvArgs
  env = FleetRepoEnv(_handle(), EnvArgs(ds={"x": 1}))
  before = fake_r2egym.core.read_namespaced_pod.call_count
  env.reset()
  # reset re-binds (one more read) and never builds a cold DockerRuntime
  assert fake_r2egym.core.read_namespaced_pod.call_count == before + 1
  assert env.runtime.container is fake_r2egym.pod


def test_make_fleet_repo_env_single_runtime_no_cold_start(fake_r2egym, tmp_path):
  files = [tmp_path / "a.py", tmp_path / "b.py"]
  for f in files:
    f.write_text("x")
  env = adapter.make_fleet_repo_env(_handle(), command_files=[str(f) for f in files])
  assert fake_r2egym.core.read_namespaced_pod.call_count == 1
  assert env.runtime.container_name == "pod-1"
  assert env.backend == "kubernetes-sandbox"
  assert env.commands == []                       # initialized (guards step())
  assert fake_r2egym.copied_ns == ["rl-ns", "rl-ns"]   # both files, right ns


def test_make_fleet_repo_env_requires_ds(fake_r2egym):
  with pytest.raises(RuntimeError, match="keep_row=True"):
    adapter.make_fleet_repo_env(_handle(with_ds=False))


def test_build_classes_thread_safe(fake_r2egym):
  _reset_classes()
  results = []

  def build():
    results.append(adapter._build_classes())

  threads = [threading.Thread(target=build) for _ in range(8)]
  for t in threads:
    t.start()
  for t in threads:
    t.join()
  # every thread got the same memoized class objects (stable isinstance)
  assert all(r is results[0] for r in results)
