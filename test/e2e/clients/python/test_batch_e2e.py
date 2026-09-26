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

import time
import uuid
from datetime import UTC, datetime

import kubernetes
import pytest
from test.e2e.clients.python.framework.context import TestContext
from test.e2e.clients.python.test_e2e_python_sdk import (  # noqa: F401
    deploy_router,
    sandbox_coldpool,
    sandbox_template,
    sandbox_warmpool,
    tc,
    temp_namespace,
)

from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.exceptions import BatchNotFoundError
from k8s_agent_sandbox.models import SandboxLocalTunnelConnectionConfig

MEMBERS_READY_TIMEOUT_SECONDS = 120


def _batch_manifest(batch_id: str, warmpool: str) -> str:
    lease_duration = 300
    # metav1.MicroTime parses with RFC3339Micro, which requires exactly six fractional digits.
    renew_time = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    claims = "\n---\n".join(
        f"""apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: {batch_id}-{ordinal}
  labels:
    agents.x-k8s.io/batch-id: {batch_id}
  annotations:
    agents.x-k8s.io/batch-group-size: "2"
    agents.x-k8s.io/batch-group-min-ready: "2"
spec:
  warmPoolRef:
    name: {warmpool}"""
        for ordinal in (0, 1)
    )
    lease = f"""apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: batch-{batch_id}
  labels:
    agents.x-k8s.io/batch-id: {batch_id}
  annotations:
    agents.x-k8s.io/batch-lease-duration: "{lease_duration}"
spec:
  leaseDurationSeconds: {lease_duration}
  renewTime: "{renew_time}\""""
    return f"{claims}\n---\n{lease}\n"


def test_batch_get_batch_members_connect_detach(
    tc, temp_namespace, sandbox_warmpool, deploy_router
):
    batch_id = f"b{uuid.uuid4().hex[:10]}"
    tc.apply_manifest_text(_batch_manifest(batch_id, sandbox_warmpool), namespace=temp_namespace)

    config = SandboxLocalTunnelConnectionConfig(router_namespace=temp_namespace)
    client = SandboxClient(connection_config=config)
    try:
        batch = client.get_batch(batch_id, namespace=temp_namespace)

        deadline = time.monotonic() + MEMBERS_READY_TIMEOUT_SECONDS
        while True:
            members = batch.members()
            if len(members) == 2 and all(m.ready for m in members):
                break
            if time.monotonic() > deadline:
                pytest.fail(f"batch members did not become ready in time: {members}")
            time.sleep(1)

        sandbox = batch.connect(members[0])
        result = sandbox.commands.run("echo 'Hello from batch'")
        assert result.stdout == "Hello from batch\n"
        assert result.exit_code == 0

        batch.detach()

        custom_objects_api = tc.get_custom_objects_api()
        for ordinal in (0, 1):
            claim = custom_objects_api.get_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1beta1",
                namespace=temp_namespace,
                plural="sandboxclaims",
                name=f"{batch_id}-{ordinal}",
            )
            assert claim is not None

        coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
        lease = coordination_api.read_namespaced_lease(f"batch-{batch_id}", temp_namespace)
        assert lease.spec.holder_identity is None
    finally:
        client.delete_all()


def test_get_batch_not_found_raises(tc, temp_namespace):
    client = SandboxClient()
    with pytest.raises(BatchNotFoundError):
        client.get_batch("bnonexistent1", namespace=temp_namespace)
