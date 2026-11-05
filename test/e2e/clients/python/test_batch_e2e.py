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

import signal
import subprocess
import sys
import time
import uuid
from datetime import UTC, datetime

import kubernetes
import pytest

from k8s_agent_sandbox import SandboxClient, batch_reaper, batch_utils
from k8s_agent_sandbox.k8s_helper import K8sHelper
from k8s_agent_sandbox.exceptions import BatchNotFoundError, BatchTimeoutError
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


def _lease_manifest(batch_id: str) -> str:
    lease_duration = 300
    # metav1.MicroTime parses with RFC3339Micro, which requires exactly six fractional digits.
    renew_time = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    return f"""apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: batch-{batch_id}
  labels:
    agents.x-k8s.io/batch-id: {batch_id}
  annotations:
    agents.x-k8s.io/batch-lease-duration: "{lease_duration}"
spec:
  leaseDurationSeconds: {lease_duration}
  renewTime: "{renew_time}"
"""


def _claims_manifest(batch_id: str, warmpool: str, lease_uid: str) -> str:
    return "\n---\n".join(
        f"""apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxClaim
metadata:
  name: {batch_id}-{ordinal}
  labels:
    agents.x-k8s.io/batch-id: {batch_id}
    agents.x-k8s.io/batch-lease-uid: {lease_uid}
  annotations:
    agents.x-k8s.io/batch-group-size: "2"
    agents.x-k8s.io/batch-group-min-ready: "2"
spec:
  warmPoolRef:
    name: {warmpool}"""
        for ordinal in (0, 1)
    )


def test_batch_get_batch_members_connect_detach(
    tc, temp_namespace, sandbox_warmpool, deploy_router
):
    batch_id = f"b{uuid.uuid4().hex[:10]}"
    coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
    tc.apply_manifest_text(_lease_manifest(batch_id), namespace=temp_namespace)
    lease = coordination_api.read_namespaced_lease(f"batch-{batch_id}", temp_namespace)
    tc.apply_manifest_text(
        _claims_manifest(batch_id, sandbox_warmpool, lease.metadata.uid),
        namespace=temp_namespace,
    )

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


def test_acquire_adds_members_and_continues_ordinals_after_detach(tc, temp_namespace, sandbox_warmpool):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    client = SandboxClient()
    try:
        batch = client.claim_batch(
            [BatchGroup(warmpool=sandbox_warmpool, size=1), BatchGroup(warmpool="unready-warmpool", size=0)],
            namespace=temp_namespace,
        )
        batch_id = batch.batch_id
        batch_selector = f"agents.x-k8s.io/batch-id={batch_id}"
        member = batch.acquire(sandbox_warmpool, timeout=MEMBERS_READY_TIMEOUT_SECONDS)
        assert (member.claim_name, member.state) == (f"{batch_id}-1", MemberState.READY)
        # It carries its pool's group label, so release_group() frees it with the pool's initial claim.
        group_selector = batch_utils.group_label_selector(batch_id, sandbox_warmpool)
        _wait_for_claims(tc, temp_namespace, group_selector, [f"{batch_id}-0", f"{batch_id}-1"])

        with pytest.raises(BatchTimeoutError):
            batch.acquire("unready-warmpool", timeout=10)
        _wait_for_claims(tc, temp_namespace, batch_selector, [f"{batch_id}-0", f"{batch_id}-1"])

        batch.detach()
        batch = client.get_batch(batch_id, namespace=temp_namespace)
        # Ordinal 2 went to the claim that timed out, so it isn't reused.
        member = batch.acquire(sandbox_warmpool, timeout=MEMBERS_READY_TIMEOUT_SECONDS)
        assert member.claim_name == f"{batch_id}-3"
        assert batch.err() is None
        batch.release()
        _wait_for_claims(tc, temp_namespace, batch_selector, [])
    finally:
        client.delete_all()


def test_get_batch_not_found_raises(tc, temp_namespace):
    client = SandboxClient()
    with pytest.raises(BatchNotFoundError):
        client.get_batch("bnonexistent1", namespace=temp_namespace)


# Short enough that a test can wait out a Lease, with the smallest skew margin the reaper accepts.
_REAPER_LEASE_DURATION = 10
_REAPER_SKEW_MARGIN = batch_utils.CLOCK_SKEW_MARGIN

# A driver that claims a batch, prints its id, and either reports err() and releases on stdin or waits to be killed.
_DRIVER_SCRIPT = """
import sys, time
from k8s_agent_sandbox import BatchGroup, SandboxClient
batch = SandboxClient().claim_batch(
    [BatchGroup(warmpool="unready-warmpool", size=2)], namespace=sys.argv[1], lease_duration=int(sys.argv[2])
)
print(batch.batch_id, flush=True)
if sys.stdin.readline():
    print(repr(batch.err()), flush=True)
    batch.release()
else:
    time.sleep(3600)
"""


def _read_lease(tc, namespace: str, batch_id: str):
    coordination_api = kubernetes.client.CoordinationV1Api(tc.get_api_client())
    try:
        return coordination_api.read_namespaced_lease(f"batch-{batch_id}", namespace)
    except kubernetes.client.ApiException as e:
        if e.status == 404:
            return None
        raise


def _reap_until_reaped(namespace: str, batch_id: str, timeout: float) -> float:
    """Runs the reaper until it reaps ``batch_id`` and returns when it did, on the monotonic clock."""
    helper = K8sHelper()
    deadline = time.monotonic() + timeout
    while True:
        reaped, errors = batch_reaper.reap(helper, [namespace], _REAPER_SKEW_MARGIN)
        assert errors == 0
        if (namespace, batch_id) in reaped:
            return time.monotonic()
        if time.monotonic() > deadline:
            pytest.fail(f"batch '{batch_id}' was not reaped in time")
        time.sleep(1)


def test_reaper_reaps_a_killed_drivers_batch_and_leaves_a_live_one(tc, temp_namespace):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    client = SandboxClient()
    driver = subprocess.Popen(
        [sys.executable, "-c", _DRIVER_SCRIPT, temp_namespace, str(_REAPER_LEASE_DURATION)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        text=True,
    )
    try:
        live = client.claim_batch(
            [BatchGroup(warmpool="unready-warmpool", size=1)],
            namespace=temp_namespace,
            lease_duration=_REAPER_LEASE_DURATION,
        )
        dead_id = driver.stdout.readline().strip()
        _wait_for_claims(tc, temp_namespace, f"agents.x-k8s.io/batch-id={dead_id}", [f"{dead_id}-0", f"{dead_id}-1"])
        driver.kill()
        driver.wait()
        killed_at = time.monotonic()

        # The Lease was renewed at most one renew interval before the kill.
        reaped_at = _reap_until_reaped(temp_namespace, dead_id, timeout=120)
        renew_interval = _REAPER_LEASE_DURATION // 3
        assert reaped_at - killed_at >= _REAPER_LEASE_DURATION + _REAPER_SKEW_MARGIN - renew_interval
        assert _read_lease(tc, temp_namespace, dead_id) is None
        _wait_for_claims(tc, temp_namespace, f"agents.x-k8s.io/batch-id={dead_id}", [])

        assert live.err() is None
        assert _read_lease(tc, temp_namespace, live.batch_id) is not None
        _wait_for_claims(tc, temp_namespace, f"agents.x-k8s.io/batch-id={live.batch_id}", [f"{live.batch_id}-0"])
    finally:
        driver.kill()
        driver.wait()
        client.delete_all()


def test_reaper_leaves_a_paused_drivers_batch_within_lease_duration(tc, temp_namespace):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    lease_duration = 30
    pause = 11
    driver = subprocess.Popen(
        [sys.executable, "-c", _DRIVER_SCRIPT, temp_namespace, str(lease_duration)],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        text=True,
    )
    try:
        batch_id = driver.stdout.readline().strip()
        selector = f"agents.x-k8s.io/batch-id={batch_id}"
        _wait_for_claims(tc, temp_namespace, selector, [f"{batch_id}-0", f"{batch_id}-1"])

        driver.send_signal(signal.SIGSTOP)
        time.sleep(pause)
        stopped_lease = _read_lease(tc, temp_namespace, batch_id)
        assert stopped_lease is not None

        reaped, errors = batch_reaper.reap(K8sHelper(), [temp_namespace], _REAPER_SKEW_MARGIN)
        assert (reaped, errors) == ([], 0)
        assert _read_lease(tc, temp_namespace, batch_id) is not None
        _wait_for_claims(tc, temp_namespace, selector, [f"{batch_id}-0", f"{batch_id}-1"])

        driver.send_signal(signal.SIGCONT)
        deadline = time.monotonic() + lease_duration
        while True:
            lease = _read_lease(tc, temp_namespace, batch_id)
            assert lease is not None
            if lease.metadata.resource_version != stopped_lease.metadata.resource_version:
                break
            if time.monotonic() > deadline:
                pytest.fail(f"resumed driver did not renew batch '{batch_id}' in time")
            time.sleep(0.5)

        driver.stdin.write("release\n")
        driver.stdin.flush()
        assert driver.stdout.readline().strip() == "None"
        assert driver.wait(timeout=60) == 0
        assert _read_lease(tc, temp_namespace, batch_id) is None
        _wait_for_claims(tc, temp_namespace, selector, [])
    finally:
        driver.kill()
        driver.wait()


def test_reaper_waits_out_a_detach_grace(tc, temp_namespace):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    client = SandboxClient()
    grace = 20
    batch = client.claim_batch(
        [BatchGroup(warmpool="unready-warmpool", size=1)],
        namespace=temp_namespace,
        lease_duration=_REAPER_LEASE_DURATION,
    )
    batch_id = batch.batch_id
    _wait_for_claims(tc, temp_namespace, f"agents.x-k8s.io/batch-id={batch_id}", [f"{batch_id}-0"])
    batch.detach(grace=grace)
    detached_at = time.monotonic()

    reaped_at = _reap_until_reaped(temp_namespace, batch_id, timeout=120)
    assert reaped_at - detached_at >= grace + _REAPER_SKEW_MARGIN
    _wait_for_claims(tc, temp_namespace, f"agents.x-k8s.io/batch-id={batch_id}", [])


def test_deleting_a_batch_lease_garbage_collects_its_claims(tc, temp_namespace):
    tc.apply_manifest_text(_UNREADY_POOL_MANIFEST, namespace=temp_namespace)
    client = SandboxClient()
    batch = client.claim_batch([BatchGroup(warmpool="unready-warmpool", size=2)], namespace=temp_namespace)
    batch_id = batch.batch_id
    selector = f"agents.x-k8s.io/batch-id={batch_id}"
    _wait_for_claims(tc, temp_namespace, selector, [f"{batch_id}-0", f"{batch_id}-1"])
    batch.detach()
    lease = _read_lease(tc, temp_namespace, batch_id)
    claims = tc.get_custom_objects_api().list_namespaced_custom_object(
        group="extensions.agents.x-k8s.io",
        version="v1beta1",
        namespace=temp_namespace,
        plural="sandboxclaims",
        label_selector=selector,
    )["items"]
    for claim in claims:
        assert [(o["kind"], o["name"], o["uid"]) for o in claim["metadata"]["ownerReferences"]] == [
            ("Lease", f"batch-{batch_id}", lease.metadata.uid)
        ]

    # No reaper and no release(), so only the garbage collector deletes the claims.
    kubernetes.client.CoordinationV1Api(tc.get_api_client()).delete_namespaced_lease(
        f"batch-{batch_id}", temp_namespace
    )
    _wait_for_claims(tc, temp_namespace, selector, [])
