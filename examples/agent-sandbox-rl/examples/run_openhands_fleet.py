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

"""Fan out OpenHands conversations onto Agent Sandbox warm pools.

The OpenHands twin of run_swebench_fleet.py: the fleet warms one pool of
agent-server pods, then runs N concurrent "rollouts". Each rollout binds the
fleet-acquired pod into an `AgentSandboxWorkspace` via
`adapters.openhands.make_handle_workspace` — the fleet owns every pod
(acquire/release around the rollout); the workspace only binds.

With LLM_API_KEY set each rollout is a real OpenHands conversation (needs the
`openhands-tools` package for the default agent); without it, each rollout is a
workspace smoke (server info + shell command), so the fan-out is testable with
cluster access alone. Env-configured:

  N_CONVERSATIONS=4 MAX_CONCURRENT=4 SANDBOX_SESSION_KEY=$(openssl rand -hex 24) \
  LLM_API_KEY=... LLM_MODEL=... NAMESPACE=default python run_openhands_fleet.py

Requires: Python >= 3.12 (the openhands-sdk floor) and
pip install -e ../../../clients/integrations/openhands  (plus openhands-tools
for the conversation mode). The agent-server image tag should match the
installed openhands-sdk release.
"""

import json
import logging
import os

from agent_sandbox_rl import (
    ClusterConfig,
    FleetConfig,
    ResourceSpec,
    SandboxFleet,
    Task,
    TemplateSpec,
)
from agent_sandbox_rl.adapters.openhands import make_handle_workspace

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

DEFAULT_IMAGE = "ghcr.io/openhands/agent-server:1.44.1-python"
# The image's ENTRYPOINT, restated: TemplateSpec.keepalive_command renders as
# the container *command* (it replaces the entrypoint), so the server must be
# launched explicitly.
SERVER_COMMAND = [
    "tini", "--", "/agent-server/.venv/bin/python", "-m",
    "openhands.agent_server", "--host", "0.0.0.0", "--port", "8000",
]
DEFAULT_PROMPT = (
    "Write a haiku about pre-warmed sandboxes into /workspace/haiku.txt, "
    "then print the file."
)


def _env(name, default):
  return os.getenv(name, default)


def _pool_size(max_concurrent, pool_cap, *, explicit):
  """Per-image pool cap: MAX_WARMPOOL_SIZE, raised to MAX_CONCURRENT if lower.

  FleetConfig.max_warmpool_size hard-caps replicas per image pool and every
  active conversation holds a pod, so a cap below the concurrency would
  silently throttle claims. Raising it means more warm capacity than asked
  for — say so, and loudly when the cap was set explicitly.
  """
  if pool_cap >= max_concurrent:
    return pool_cap
  logging.log(
      logging.WARNING if explicit else logging.INFO,
      "MAX_WARMPOOL_SIZE=%d (%s) is below MAX_CONCURRENT=%d; using %d so every "
      "concurrent conversation can hold a warm pod — expect that many replicas",
      pool_cap, "explicit" if explicit else "default", max_concurrent,
      max_concurrent)
  return max_concurrent


def _agent_server_container(session_key):
  """Merged into the rendered container via extra_pod_spec (no name = first).

  The readinessProbe is load-bearing: it makes pool-Ready mean
  "agent-server is serving", which is what lets claims bind instantly and the
  workspace keep its short health budget.
  """
  container = {
      "ports": [{"containerPort": 8000}],
      "readinessProbe": {
          "httpGet": {"path": "/health", "port": 8000},
          # 5 min boot budget: agent-server cold boots were measured at up to
          # ~4 min on runc nodes. This is pool-fill time, off the claim path.
          "periodSeconds": 2,
          "failureThreshold": 150,
      },
  }
  if session_key:
    # Pool-level key (pre-warmed servers cannot take a per-claim key). The
    # example inlines the value for brevity; production should use
    # valueFrom.secretKeyRef — see the integration's README.
    container["env"] = [{"name": "OH_SESSION_API_KEYS_0", "value": session_key}]
  return container


def _conversation_mode():
  """LLM configured AND the agent preset installed; else fall back to smoke."""
  if not os.getenv("LLM_API_KEY"):
    return False
  try:
    import openhands.tools  # noqa: F401
  except ImportError:
    logging.warning(
        "LLM_API_KEY is set but openhands-tools is not installed — falling "
        "back to smoke mode (pip install openhands-tools)")
    return False
  return True


def _rollout(session_key, conversation_mode):
  """Per-task fn for fleet.run: bind the handle, converse (or smoke)."""
  def fn(task, handle):
    workspace = make_handle_workspace(handle, api_key=session_key or None)
    try:
      if conversation_mode:
        return _conversation(task, workspace)
      result = workspace.execute_command("echo ready && uname -m")
      return {"id": task.id, "mode": "smoke", "cluster": handle.cluster_name,
              "exit_code": result.exit_code, "output": result.stdout.strip()}
    finally:
      workspace.cleanup()   # no-op on the pod; the fleet releases the handle
  return fn


def _conversation(task, workspace):
  from pydantic import SecretStr

  from openhands.sdk import LLM, Conversation
  from openhands.tools.preset.default import get_default_agent

  llm = LLM(
      usage_id="agent",
      model=os.getenv("LLM_MODEL", "gpt-5.5"),
      base_url=os.getenv("LLM_BASE_URL"),
      api_key=SecretStr(os.environ["LLM_API_KEY"]),
  )
  agent = get_default_agent(llm=llm, cli_mode=True)
  conversation = Conversation(agent=agent, workspace=workspace)
  try:
    conversation.send_message(_env("PROMPT", DEFAULT_PROMPT))
    conversation.run()
    return {"id": task.id, "mode": "conversation",
            "status": str(conversation.state.execution_status)}
  finally:
    conversation.close()


def main():
  n_conversations = int(_env("N_CONVERSATIONS", "2"))
  max_concurrent = int(_env("MAX_CONCURRENT", "2"))
  namespace = _env("NAMESPACE", "default")
  image = _env("AGENT_SERVER_IMAGE", DEFAULT_IMAGE)
  session_key = _env("SANDBOX_SESSION_KEY", "")

  node_selector = None
  if _env("NODE_SELECTOR_KEY", "") and _env("NODE_SELECTOR_VAL", ""):
    node_selector = {os.environ["NODE_SELECTOR_KEY"]: os.environ["NODE_SELECTOR_VAL"]}

  template = TemplateSpec(
      keepalive_command=SERVER_COMMAND,
      runtime_class=_env("RUNTIME_CLASS", "") or None,
      node_selector=node_selector,
      resources=ResourceSpec(
          cpu=_env("SANDBOX_CPU", "500m"), memory=_env("SANDBOX_MEM", "1Gi")),
      extra_pod_spec={"containers": [_agent_server_container(session_key)]},
  )

  contexts = [c for c in _env("KUBE_CONTEXTS", "").split(",") if c]
  if contexts:
    clusters = [ClusterConfig(name=c, context=c, namespace=namespace)
                for c in contexts]
  else:
    clusters = [ClusterConfig(name="default", namespace=namespace)]

  # Pool depth must cover the concurrency (default cap 8); see _pool_size.
  pool_cap_env = os.getenv("MAX_WARMPOOL_SIZE")
  max_pool = _pool_size(max_concurrent, int(pool_cap_env) if pool_cap_env else 8,
                        explicit=bool(pool_cap_env))
  fleet = SandboxFleet(FleetConfig(
      clusters=clusters, max_concurrent=max_concurrent, template=template,
      max_warmpool_size=max_pool,
      ready_timeout=int(_env("SANDBOX_READY_TIMEOUT", "600"))))
  # One shared agent-server image -> the fleet plans a single warm pool sized
  # to the concurrency; conversations are just tasks against it.
  fleet.load_tasks([Task(id=f"conv-{i}", image=image, metadata={})
                    for i in range(n_conversations)])

  results = fleet.run(_rollout(session_key, _conversation_mode()),
                      strategy=_env("WARMPOOL_STRATEGY", "naive"),
                      concurrency=max_concurrent)
  print(json.dumps({"conversations": len(results), "results": results},
                   indent=2, default=str))


if __name__ == "__main__":
  main()
