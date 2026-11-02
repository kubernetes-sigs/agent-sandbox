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

from k8s_agent_sandbox import SandboxClient, batch_utils
from k8s_agent_sandbox.exceptions import BatchNotFoundError
from k8s_agent_sandbox.models import BatchEventType, BatchGroup, MemberState, SandboxLocalTunnelConnectionConfig

MEMBERS_READY_TIMEOUT_SECONDS = 120

# A warm pool with no replicas whose template image never pulls, so its claims stay not Ready.
_UNREADY_POOL_MANIFEST = """apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata:
  name: unready-template
spec:
  podTemplate:
    spec:
      containers:
      - name: unready
        image: registry.invalid/unready:0
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: unready-warmpool
spec:
  replicas: 0
  sandboxTemplateRef:
    name: unready-template
"""


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
            if len(members) == 2 and all(m.state is MemberState.READY for m in members):
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


def test_claim_batch_ready_group_connect_release(
    tc, temp_namespace, sandbox_warmpool, deploy_router
):
    config = SandboxLocalTunnelConnectionConfig(router_namespace=temp_namespace)
    client = SandboxClient(connection_config=config)
    try:
        batch = client.claim_batch(
            [BatchGroup(warmpool=sandbox_warmpool, size=2)],
            namespace=temp_namespace,
            quorum_timeout=MEMBERS_READY_TIMEOUT_SECONDS,
        )

        groups = list(batch.group_outcomes())
        assert batch.err() is None
        assert len(groups) == 1
        assert groups[0].error is None, groups[0].error
        assert len(groups[0].members) == 2

        sandbox = batch.connect(groups[0].members[0])
        result = sandbox.commands.run("echo 'Hello from batch'")
        assert result.stdout == "Hello from batch\n"
        assert result.exit_code == 0

        # Both members went to the group, so there's no MEMBER_READY left for events().
        assert [e for e in batch.events() if e.type == BatchEventType.MEMBER_READY] == []

        batch.release()

        custom_objects_api = tc.get_custom_objects_api()
        deadline = time.monotonic() + MEMBERS_READY_TIMEOUT_SECONDS
        while True:
            claims = custom_objects_api.list_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1beta1",
                namespace=temp_namespace,
                plural="sandboxclaims",
                label_selector=f"agents.x-k8s.io/batch-id={batch.batch_id}",
            )["items"]
            if not claims:
                break
            if time.monotonic() > deadline:
                pytest.fail(f"batch claims were not deleted in time: {claims}")
            time.sleep(1)

        coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
        with pytest.raises(kubernetes.client.ApiException) as e:
            coordination_api.read_namespaced_lease(f"batch-{batch.batch_id}", temp_namespace)
        assert e.value.status == 404
    finally:
        client.delete_all()


def test_claim_batch_wait_for_quorum_events_release(
    tc, temp_namespace, sandbox_warmpool, deploy_router
):
    config = SandboxLocalTunnelConnectionConfig(router_namespace=temp_namespace)
    client = SandboxClient(connection_config=config)
    try:
        # The warm pool has two replicas, so the third claim is created from the template.
        batch = client.claim_batch(
            [BatchGroup(warmpool=sandbox_warmpool, size=3, min_ready=2)],
            namespace=temp_namespace,
            quorum_timeout=MEMBERS_READY_TIMEOUT_SECONDS,
        )

        quorum = batch.wait_for_quorum()
        assert len(quorum) == 2

        sandbox = batch.connect(quorum[0])
        result = sandbox.commands.run("echo 'Hello from batch'")
        assert result.stdout == "Hello from batch\n"
        assert result.exit_code == 0

        ready = [e.member for e in batch.events() if e.type == BatchEventType.MEMBER_READY]
        assert batch.err() is None
        assert sorted(m.claim_name for m in quorum + ready) == [f"{batch.batch_id}-{i}" for i in range(3)]

        batch.release()

        custom_objects_api = tc.get_custom_objects_api()
        deadline = time.monotonic() + MEMBERS_READY_TIMEOUT_SECONDS
        while True:
            claims = custom_objects_api.list_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1beta1",
                namespace=temp_namespace,
                plural="sandboxclaims",
                label_selector=f"agents.x-k8s.io/batch-id={batch.batch_id}",
            )["items"]
            if not claims:
                break
            if time.monotonic() > deadline:
                pytest.fail(f"batch claims were not deleted in time: {claims}")
            time.sleep(1)

        coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
        with pytest.raises(kubernetes.client.ApiException) as e:
            coordination_api.read_namespaced_lease(f"batch-{batch.batch_id}", temp_namespace)
        assert e.value.status == 404
    finally:
        client.delete_all()


def _wait_for_claims(tc, namespace: str, label_selector: str, want: list[str]) -> None:
    custom_objects_api = tc.get_custom_objects_api()
    deadline = time.monotonic() + MEMBERS_READY_TIMEOUT_SECONDS
    while True:
        claims = custom_objects_api.list_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1beta1",
            namespace=namespace,
            plural="sandboxclaims",
            label_selector=label_selector,
        )["items"]
        names = sorted(c["metadata"]["name"] for c in claims)
        if names == want:
            return
        if time.monotonic() > deadline:
            pytest.fail(f"claims for '{label_selector}' are {names}, want {want}")
        time.sleep(1)


def test_claim_batch_release_not_ready_then_release_group(tc, temp_namespace, sandbox_warmpool):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    client = SandboxClient()
    try:
        batch = client.claim_batch(
            [BatchGroup(warmpool=sandbox_warmpool, size=2), BatchGroup(warmpool="unready-warmpool", size=1)],
            namespace=temp_namespace,
            quorum_timeout=MEMBERS_READY_TIMEOUT_SECONDS,
        )
        batch_selector = f"agents.x-k8s.io/batch-id={batch.batch_id}"
        deadline = time.monotonic() + MEMBERS_READY_TIMEOUT_SECONDS
        while True:
            members = batch.members()
            ready = sorted(m.claim_name for m in members if m.state is MemberState.READY)
            if len(members) == 3 and len(ready) == 2:
                break
            if time.monotonic() > deadline:
                pytest.fail(f"batch members did not become ready in time: {members}")
            time.sleep(1)

        batch.release_not_ready()
        _wait_for_claims(tc, temp_namespace, batch_selector, ready)

        # Selected by the hashed group label, as release_group's deletecollection is.
        _wait_for_claims(
            tc, temp_namespace, batch_utils.group_label_selector(batch.batch_id, sandbox_warmpool), ready
        )
        batch.release_group(sandbox_warmpool)
        _wait_for_claims(tc, temp_namespace, batch_selector, [])
        assert batch.err() is None

        coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
        coordination_api.read_namespaced_lease(f"batch-{batch.batch_id}", temp_namespace)
        batch.release()
        with pytest.raises(kubernetes.client.ApiException) as e:
            coordination_api.read_namespaced_lease(f"batch-{batch.batch_id}", temp_namespace)
        assert e.value.status == 404
    finally:
        client.delete_all()


def test_get_batch_not_found_raises(tc, temp_namespace):
    client = SandboxClient()
    with pytest.raises(BatchNotFoundError):
        client.get_batch("bnonexistent1", namespace=temp_namespace)
