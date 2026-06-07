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

"""Minimal dispatcher: Anthropic work queue -> SandboxClaim -> warm pod.

Trimmed for readability. The production version (with stale-claim reaping,
retries, and argparse wiring) lives in
GoogleCloudPlatform/kubernetes-engine-samples/ai-ml/anthropic-agent-sandbox.
"""
import json
import os
import urllib.request

import anthropic
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import SandboxInClusterConnectionConfig

ENV_ID = os.environ["ANTHROPIC_ENVIRONMENT_ID"]
NAMESPACE = os.environ.get("SANDBOX_NAMESPACE", "agent-sandbox")
DISPATCH_PORT = int(os.environ.get("DISPATCH_PORT", "8080"))

ant = anthropic.Anthropic(auth_token=os.environ["ANTHROPIC_ENVIRONMENT_KEY"])
sbx = SandboxClient(
    connection_config=SandboxInClusterConnectionConfig(
        use_pod_ip=True, server_port=DISPATCH_PORT
    )
)

while True:
    item = ant.beta.environments.work.poll(ENV_ID, block_ms=900)
    if item is None:
        continue
    session_id, work_id = item.data.id, item.id

    sb = sbx.create_sandbox(
        template="claude-agent-worker",
        namespace=NAMESPACE,
        warmpool="claude-agent-worker",
        labels={"anthropic.com/session-id": session_id},
        sandbox_ready_timeout=120,
    )
    # On GKE the per-claim Service DNS is too fresh to resolve and
    # sb.get_pod_ip() returns None; read the bound pod's IP directly.
    pod = sb.k8s_helper.core_v1_api.read_namespaced_pod(sb.get_pod_name(), sb.namespace)
    urllib.request.urlopen(
        urllib.request.Request(
            f"http://{pod.status.pod_ip}:{DISPATCH_PORT}/",
            data=json.dumps({"session_id": session_id, "work_id": work_id}).encode(),
            headers={"Content-Type": "application/json"},
        ),
        timeout=10,
    )
