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

"""What survives ``resume()`` when the original claim is gone.

When the claim has expired the provider provisions a replacement. Everything the caller
configured on the original ``create()`` has to come back with it — the replacement is
supposed to be a continuation, not a fresh sandbox with default settings.
"""

from __future__ import annotations

import logging
import types
import uuid
from pathlib import Path

import pytest
from agents.sandbox.manifest import Manifest
from agents.sandbox.session import SandboxSessionState
from agents.sandbox.snapshot import resolve_snapshot
from k8s_agent_sandbox.exceptions import SandboxNotFoundError

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient
from support import NAMESPACE, make_options


async def _create_then_lose_claim(
    client: K8sSandboxClient,
    fake_client: FakeAsyncSandboxClient,
    workspace: Path,
    **option_overrides: object,
) -> SandboxSessionState:
    """Create a session, close it, then make its claim vanish (TTL expiry, node loss)."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(**option_overrides),
    )
    async with session:
        state = session.state

    fake_client.drop_claim(state.claim_name, namespace=NAMESPACE)
    return state


async def test_replacement_preserves_lifecycle_options(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    state = await _create_then_lose_claim(
        client,
        fake_client,
        workspace,
        shutdown_after_seconds=120,
        sandbox_ready_timeout=600,
        labels={"owner": "platform"},
        pod_labels={"team": "agents"},
    )

    resumed = await client.resume(state)

    assert len(fake_client.created) == 2
    original, replacement = fake_client.created
    assert replacement == original

    await client.delete(resumed)


async def test_replacement_preserves_disabled_ttl(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """``None`` means "no TTL backstop" — it must not silently become the 3600s default."""

    state = await _create_then_lose_claim(
        client, fake_client, workspace, shutdown_after_seconds=None
    )

    resumed = await client.resume(state)

    assert fake_client.created[1]["shutdown_after_seconds"] is None

    await client.delete(resumed)


async def test_replacement_keeps_pinned_exposed_port_host(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """A caller-supplied host is a deliberate override; the replacement must keep it."""

    state = await _create_then_lose_claim(
        client,
        fake_client,
        workspace,
        exposed_ports=(8080,),
        exposed_port_host="gateway.example.test",
    )
    assert state.exposed_port_host == "gateway.example.test"

    resumed = await client.resume(state)

    endpoint = await resumed.resolve_exposed_port(8080)
    assert endpoint.host == "gateway.example.test"

    await client.delete(resumed)


async def test_replacement_refreshes_unpinned_host_to_new_sandbox(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Without an override the host is derived, so it has to follow the new sandbox."""

    state = await _create_then_lose_claim(client, fake_client, workspace, exposed_ports=(8080,))
    original_host = state.exposed_port_host
    assert original_host == f"{state.sandbox_id}.{NAMESPACE}.svc.cluster.local"

    resumed = await client.resume(state)

    expected = f"{resumed.state.sandbox_id}.{NAMESPACE}.svc.cluster.local"
    assert resumed.state.exposed_port_host == expected
    assert resumed.state.exposed_port_host != original_host

    await client.delete(resumed)


async def test_missing_claim_provisions_a_replacement(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    state = await _create_then_lose_claim(client, fake_client, workspace)
    original_claim = state.claim_name

    fake_client.fail_get_sandbox = SandboxNotFoundError("claim is gone")
    resumed = await client.resume(state)

    assert resumed.state.claim_name != original_claim
    assert len(fake_client.created) == 2

    await client.delete(resumed)


async def test_unexpected_reattach_failure_is_not_swallowed(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """An API blip is not proof the sandbox died. Replacing it would orphan a live pod."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )
    async with session:
        state = session.state

    fake_client.fail_get_sandbox = RuntimeError("kube-apiserver unavailable")

    with pytest.raises(RuntimeError, match="kube-apiserver"):
        await client.resume(state)

    # The original sandbox is still live and must not have been abandoned.
    assert len(fake_client.created) == 1

    fake_client.fail_get_sandbox = None
    await client.delete(session)


async def test_state_written_by_an_older_version_still_deserializes(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Session state is persisted JSON, so a payload missing the newer keys must load."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )
    payload = client.serialize_session_state(session.state)
    for added_later in (
        "exposed_port_host_override",
        "sandbox_ready_timeout",
        "shutdown_after_seconds",
        "labels",
        "pod_labels",
    ):
        payload.pop(added_later, None)

    state = client.deserialize_session_state(payload)

    assert state.sandbox_ready_timeout == 180
    assert state.shutdown_after_seconds == 3600
    assert state.labels is None
    assert state.pod_labels is None
    assert state.exposed_port_host_override is None

    await client.delete(session)


async def test_resume_refuses_foreign_state(client: K8sSandboxClient) -> None:
    foreign = SandboxSessionState(
        type="not-k8s",
        session_id=uuid.uuid4(),
        manifest=Manifest(),
        snapshot=resolve_snapshot(None, "foreign"),
    )

    with pytest.raises(TypeError, match="K8sSandboxSessionState"):
        await client.resume(foreign)


async def test_delete_refuses_foreign_session(client: K8sSandboxClient) -> None:
    foreign = types.SimpleNamespace(_inner=object())

    with pytest.raises(TypeError, match="K8sSandboxSession"):
        await client.delete(foreign)  # type: ignore[arg-type]


async def test_delete_surfaces_a_failed_claim_deletion(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """A claim the API server refused to delete leaves a pod and a PVC running."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )
    sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
    sandbox.fail_terminate = RuntimeError("claim deletion refused")

    with pytest.raises(RuntimeError, match="claim deletion refused"):
        await client.delete(session)

    sandbox.fail_terminate = None
    await client.delete(session)


async def test_delete_logs_a_failed_teardown(
    client: K8sSandboxClient, workspace: Path, caplog: pytest.LogCaptureFixture
) -> None:
    """Deletion carries on, but a crash in shutdown() must not vanish."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async def broken_shutdown() -> None:
        raise RuntimeError("transport gone")

    session._inner.shutdown = broken_shutdown  # type: ignore[method-assign]

    with caplog.at_level(logging.WARNING, logger="openai_agents_k8s_sandbox.client"):
        await client.delete(session)

    assert "teardown failed" in caplog.text
    assert "transport gone" in caplog.text


async def test_delete_accepts_an_already_deleted_claim(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Nothing is left to free, so deleting twice is not an error."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )
    fake_client.drop_claim(session.state.claim_name, namespace=NAMESPACE)

    await client.delete(session)


async def test_delete_refuses_an_unwrapped_session(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """`_inner` is the wrapper's attribute; the session it wraps has none of its own."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    with pytest.raises(TypeError, match="K8sSandboxSession"):
        await client.delete(session._inner)

    await client.delete(session)
