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

"""R2E-Gym adapter: run R2E-Gym's `RepoEnv` on a fleet-pre-warmed sandbox.

R2E-Gym's `kubernetes-sandbox` backend cold-creates a sandbox per env (via the
SDK's old context-manager `SandboxClient`). This adapter instead **binds** an
already-warm pod acquired from an `agent_sandbox_rl` `SandboxFleet` into R2E-Gym's
`DockerRuntime`/`RepoEnv`, so SWE-bench rollouts reuse the warm pool (and, since
tunix deepswe drives R2E-Gym, tunix benefits transitively).

Usage::

    from agent_sandbox_rl import SandboxFleet, FleetConfig
    from agent_sandbox_rl.adapters.swebench import SweBenchSource
    from agent_sandbox_rl.adapters.r2egym import make_fleet_repo_env, r2egym_command_files

    fleet = SandboxFleet(FleetConfig(...))
    fleet.load_tasks(SweBenchSource(limit=5, keep_row=True))   # keep_row REQUIRED

    def rollout(task, handle):
        env = make_fleet_repo_env(handle, command_files=r2egym_command_files())
        try:
            instruction = env.get_task_instruction()
            obs, reward, done, info = env.step(some_action)
            return {"id": task.id, "obs": str(obs)}
        finally:
            env.close()          # safe no-op teardown; does NOT delete the pod

    results = fleet.run(rollout, strategy="sliding")

**Ownership:** the env never deletes the pod — `env.close()` is a no-op. The fleet
owns the pod's lifecycle; `fleet.run`/`fleet.release(handle)` frees it. Namespace
flows from the handle's cluster (`ClusterConfig.namespace`): exec and file-copy are
reimplemented to target the handle's namespace explicitly and use a per-runtime
`CoreV1Api`, so concurrent rollouts across clusters/namespaces stay correct
(R2E-Gym's base hardcodes the ``default`` namespace via a module global).

Requires R2E-Gym (`pip install r2egym`, or `pip install -e` the R2E-Gym checkout).
Importing this module is cheap; the R2E-Gym subclasses are built lazily on first
use (thread-safely) so the core package imports fine without R2E-Gym installed.
"""

from __future__ import annotations

import concurrent.futures
import io
import logging
import os
import re
import tarfile
import threading
import time

from kubernetes.stream import stream

logger = logging.getLogger("agent_sandbox_rl.adapters.r2egym")

_HINT = (
    "requires R2E-Gym — `pip install r2egym` (or `pip install -e` the R2E-Gym "
    "checkout). See examples/rl_integration.md."
)

# Memoized (FleetDockerRuntime, FleetRepoEnv): built once, so isinstance() stays
# valid. Guarded by a lock for thread-safe first use (rollouts build envs in
# parallel worker threads).
_CLASSES = None
_BUILD_LOCK = threading.Lock()


def _import_r2egym():
  try:
    from r2egym.agenthub.environment.env import EnvArgs, RepoEnv  # noqa: F401
    from r2egym.agenthub.runtime import docker as docker_mod
    return docker_mod, EnvArgs, RepoEnv
  except ImportError as e:  # pragma: no cover - exercised via sys.modules in tests
    raise RuntimeError(f"agent_sandbox_rl.adapters.r2egym {_HINT}") from e


def _build_classes():
  """Build (and memoize) the R2E-Gym subclasses. Raises RuntimeError w/o r2egym.

  Thread-safe via double-checked locking so concurrent first use returns one
  stable pair of class objects.
  """
  global _CLASSES
  if _CLASSES is not None:
    return _CLASSES
  with _BUILD_LOCK:
    if _CLASSES is not None:
      return _CLASSES

    from kubernetes import client as k8s
    docker_mod, _EnvArgs, RepoEnv = _import_r2egym()
    DockerRuntime = docker_mod.DockerRuntime
    cmd_timeout = getattr(docker_mod, "CMD_TIMEOUT", 90)

    class FleetDockerRuntime(DockerRuntime):
      """A `DockerRuntime` bound to a fleet-warmed pod instead of cold-creating one.

      Constructed from an `agent_sandbox_rl.SandboxHandle`; overrides the backend
      lifecycle hooks (bind instead of create, no-op teardown) and reimplements
      exec / file-copy so they target the handle's namespace with a per-runtime
      client. Everything else in R2E-Gym's runtime (setup_env, reward, …) works
      unchanged against the warm pod.
      """

      def __init__(self, handle, *, command=("/bin/bash", "-l"), logger=None,
                   **docker_kwargs):
        self._handle = handle
        ds = (getattr(handle.task, "metadata", None) or {}).get("ds")
        if ds is None:
          raise RuntimeError(
              "Task.metadata['ds'] is required for the R2E-Gym adapter — load "
              "tasks with SweBenchSource(keep_row=True).")
        super().__init__(
            ds=ds, docker_image=handle.task.image, command=list(command),
            logger=logger, backend="kubernetes-sandbox", **docker_kwargs)
        # R2E-Gym's base start_container swallows exceptions from
        # _start_kubernetes_sandbox, so surface a failed bind clearly instead of
        # proceeding with a half-built runtime.
        if getattr(self, "container", None) is None \
            or self.container_name != handle.pod_name:
          raise RuntimeError(
              f"failed to bind warm pod '{handle.pod_name}' "
              f"(ns={handle._cluster.namespace}) into R2E-Gym runtime; see logs.")

      @property
      def _ns(self) -> str:
        return self._handle._cluster.namespace

      def _start_kubernetes_sandbox(self):
        # Bind to the warm pod from the fleet handle — no SandboxClient, no template.
        cl = self._handle._cluster
        self.sb_client = None
        self.custom_api = None
        self.container_name = self._handle.pod_name
        # Per-runtime CoreV1Api (mirrors SandboxHandle.exec's isolation): the
        # kubernetes stream() websocket exec is not thread-safe across a shared
        # ApiClient, so each runtime gets its own.
        self.client = k8s.CoreV1Api(k8s.ApiClient(cl.api_client.configuration))
        self.container = self.client.read_namespaced_pod(
            name=self.container_name, namespace=cl.namespace)
        self.logger.info("Bound warm pod '%s' (ns=%s) from fleet handle",
                         self.container_name, cl.namespace)

      def _stop_kubernetes_sandbox(self):
        # The fleet owns the pod; release happens via fleet.release(handle).
        self.logger.debug("FleetDockerRuntime: teardown is a no-op (fleet owns "
                          "pod '%s')", getattr(self, "container_name", "?"))

      # --- exec / file-copy: like the base, but namespace-explicit ---------- #
      # R2E-Gym's base _run_kubernetes / _copy_to_container_kubernetes read the
      # module-level DEFAULT_NAMESPACE ("default"). We reimplement them to use the
      # handle's namespace and per-runtime client so parallel rollouts across
      # namespaces don't race on a shared global. Bodies mirror R2E-Gym's
      # (output, exit_code) contract that patch-apply / reward grading rely on.

      def _run_kubernetes(self, code, timeout=cmd_timeout, args="", workdir=""):
        command = ""
        if workdir:
          command += f"cd {workdir} && "
        command += f"timeout {timeout} {code} {args}"
        full_command = ["/bin/sh", "-c", command]
        try:
          def execute_command():
            resp = stream(
                self.client.connect_get_namespaced_pod_exec,
                self.container_name, self._ns, command=full_command,
                stderr=True, stdin=False, stdout=True, tty=False,
                _preload_content=False)
            chunks = []
            while resp.is_open():
              resp.update(timeout=1)
              if resp.peek_stdout():
                chunks.append(resp.read_stdout())
              if resp.peek_stderr():
                chunks.append(resp.read_stderr())
            resp.close()
            return "".join(chunks), resp.returncode

          # Don't use a `with` block: on result() timeout its __exit__ would
          # shutdown(wait=True) and block on the hung exec thread. Shut down
          # without waiting instead.
          ex = concurrent.futures.ThreadPoolExecutor(max_workers=1)
          try:
            output, exit_code = ex.submit(execute_command).result(timeout=timeout + 5)
          finally:
            ex.shutdown(wait=False, cancel_futures=True)

          if exit_code is None:
            self.logger.error("Kubernetes exec: exit code not found.")
            return output, "-1"
          if exit_code == 124:
            self.logger.error("Internal timeout via 'timeout' command: %ss", timeout)
            return f"The command took too long to execute (>{timeout}s)", "-1"
          if exit_code != 0:
            self.logger.error("Kubernetes exec error: exit code %s\n%s",
                              exit_code, output)
            return output, f"Error: Exit code {exit_code}"
          output = re.sub(r"\x1b\[[0-9;]*m|\r", "", output)
          return output, str(exit_code)
        except concurrent.futures.TimeoutError:
          self.logger.error("Kubernetes exec overall timeout: %ss", timeout + 5)
          return f"The command took too long to execute (>{timeout}s)", "-1"
        except k8s.ApiException as e:
          self.logger.error("Kubernetes API error during exec: %s", e)
          return f"Error executing command in pod: {repr(e)}", "-1"
        except Exception as e:  # noqa: BLE001 — mirror base's catch-all
          self.logger.error("Unexpected error during Kubernetes exec: %s", repr(e))
          return f"Error: {repr(e)}", "-1"

      def _copy_to_container_kubernetes(self, src_path, dest_path):
        dest_dir = os.path.dirname(dest_path)
        tar_stream = io.BytesIO()
        with tarfile.open(fileobj=tar_stream, mode="w") as tar:
          tar.add(src_path, arcname=os.path.basename(dest_path))
        tar_stream.seek(0)
        max_retries, retry_delay = 5, 5
        for attempt in range(max_retries):
          try:
            resp = stream(
                self.client.connect_get_namespaced_pod_exec,
                self.container_name, self._ns,
                command=["tar", "xmf", "-", "-C", dest_dir],
                stderr=True, stdin=True, stdout=True, tty=False,
                _preload_content=False)
            resp.write_stdin(tar_stream.read())
            resp.close()
            return
          except Exception as e:  # noqa: BLE001 — mirror base's retry
            if attempt < max_retries - 1:
              self.logger.warning("copy to pod failed (attempt %d/%d): %s",
                                  attempt + 1, max_retries, e)
              time.sleep(retry_delay)
              retry_delay = min(retry_delay * 2, 60)
              tar_stream.seek(0)
            else:
              self.logger.error("copy to pod failed after %d attempts: %s",
                                max_retries, e)
              raise

    class FleetRepoEnv(RepoEnv):
      """`RepoEnv` whose runtime is a `FleetDockerRuntime` (no cold pod start).

      Mirrors `RepoEnv.__init__` but swaps the runtime construction so no throwaway
      sandbox is created. One episode per acquired handle is the intended pattern.
      """

      def __init__(self, handle, args, *, logger=None, verbose=True,
                   step_timeout=90, reward_timeout=300):
        if logger is None:
          from r2egym.agenthub.utils.log import get_logger
          self.logger = get_logger("FleetRepoEnv")
        else:
          self.logger = logger
        if not verbose:
          self.logger.setLevel(logging.CRITICAL)

        self.runtime = FleetDockerRuntime(handle, logger=self.logger)

        self.args = args
        self.done = False
        self.observation = None
        self.state = None
        self.commands = []           # base sets this only in add_commands; guard step()
        from r2egym.agenthub.agent.commands import ParseCommandBash
        self.cmd_parser = ParseCommandBash()
        self.backend = "kubernetes-sandbox"
        self.step_timeout = step_timeout
        self.reward_timeout = reward_timeout
        self.logger.info("Initialized FleetRepoEnv: %s image: %s",
                         self.runtime.repo_name, self.runtime.docker_image)

      def reset(self):
        """Soft reset: re-validate the warm-pod binding (no cold start, no setup
        rerun). For a fresh episode, prefer `fleet.release(handle)` +
        `fleet.acquire(task)` to get a clean pod."""
        self.logger.info("FleetRepoEnv soft reset (re-binding warm pod)")
        self.runtime.start_container(
            self.runtime.docker_image, self.runtime.command,
            self.runtime.container_name)
        if getattr(self.runtime, "container", None) is None:
          raise RuntimeError("FleetRepoEnv.reset: warm pod re-bind failed")
        self.observation = "Environment reset"
        self.state = None
        self.done = False
        return self.observation

  _CLASSES = (FleetDockerRuntime, FleetRepoEnv)
  return _CLASSES


def make_fleet_repo_env(handle, *, command_files=None, verbose=False,
                        step_timeout: int = 90, reward_timeout: int = 300):
  """Build an R2E-Gym `RepoEnv` bound to a fleet-warmed pod (`handle`).

  ``handle`` must come from a fleet whose tasks were loaded with
  ``SweBenchSource(keep_row=True)`` (the env/reward grading need the full dataset
  row in ``task.metadata['ds']``). ``command_files`` are R2E-Gym tool files copied
  into the pod (see `r2egym_command_files`). Call ``env.close()`` when done — it is
  a no-op that does NOT delete the pod; the fleet releases it.
  """
  _, FleetRepoEnv = _build_classes()
  from r2egym.agenthub.environment.env import EnvArgs
  ds = (getattr(handle.task, "metadata", None) or {}).get("ds")
  if ds is None:
    raise RuntimeError(
        "Task.metadata['ds'] is required for the R2E-Gym adapter — load tasks "
        "with SweBenchSource(keep_row=True).")
  env = FleetRepoEnv(handle, EnvArgs(ds=ds), verbose=verbose,
                     step_timeout=step_timeout, reward_timeout=reward_timeout)
  if command_files:
    env.add_commands(list(command_files))
  return env


def r2egym_command_files() -> list:
  """The default R2E-Gym (`r2egym` scaffold) tool files to load into a sandbox.

  Mirrors tunix deepswe's ``R2EGYM_COMMAND_FILES`` (derived from the installed
  ``r2egym`` package). Requires R2E-Gym installed.
  """
  try:
    import r2egym
  except ImportError as e:
    raise RuntimeError(f"r2egym_command_files {_HINT}") from e
  base = os.path.join(os.path.dirname(r2egym.__file__), "agenthub", "tools")
  return [
      os.path.join(base, "r2egym", "file_editor.py"),
      os.path.join(base, "search.py"),
      os.path.join(base, "r2egym", "execute_bash.py"),
      os.path.join(base, "finish.py"),
  ]


def __getattr__(name):
  """Lazily expose the R2E-Gym subclasses (PEP 562) so e.g.
  ``from agent_sandbox_rl.adapters.r2egym import FleetRepoEnv`` triggers the build
  (and a clear RuntimeError if R2E-Gym is missing)."""
  if name == "FleetDockerRuntime":
    return _build_classes()[0]
  if name == "FleetRepoEnv":
    return _build_classes()[1]
  raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
