"""Transport-level behaviour: readiness reporting, timeouts, and error classification.

``ExecTimeoutError`` and ``ExecTransportError`` mean different things to a caller — one is
worth retrying with a longer budget, the other is not — so the boundary between them
matters.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from agents.sandbox.errors import ExecTimeoutError, ExecTransportError
from agents.sandbox.manifest import Manifest

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient
from support import NAMESPACE, make_options


async def test_running_is_true_for_a_ready_sandbox(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        assert await session.running() is True

    await client.delete(session)


async def test_running_is_false_while_the_sandbox_is_not_ready(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """Alive but still pending is not ready — only "SandboxReady" counts."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.status_value = "SandboxPending"

        assert await session.running() is False

    await client.delete(session)


async def test_running_is_false_when_the_status_call_fails(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.status_error = RuntimeError("status endpoint unreachable")

        assert await session.running() is False

    await client.delete(session)


async def test_unbounded_timeout_becomes_the_configured_default(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """The in-pod API has no "wait forever" mode, so None must map to a finite budget."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exec_timeout_default_s=42.0),
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.commands_timeouts.clear()

        await session.exec("true", timeout=None)

        assert sandbox.commands_timeouts == [42]

    await client.delete(session)


async def test_subsecond_timeout_floors_to_one_second(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    """The endpoint takes whole seconds; rounding to 0 would mean "no time at all"."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        sandbox.commands_timeouts.clear()

        await session.exec("true", timeout=0.2)

        assert sandbox.commands_timeouts == [1]

    await client.delete(session)


async def test_backend_failure_is_a_transport_error_not_a_timeout(
    client: K8sSandboxClient, fake_client: FakeAsyncSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        sandbox = await fake_client.get_sandbox(session.state.claim_name, namespace=NAMESPACE)
        await sandbox.terminate()

        with pytest.raises(ExecTransportError):
            await session.exec("true")

    await client.delete(session)


async def test_timeout_error_reports_the_requested_budget(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    async with session:
        with pytest.raises(ExecTimeoutError) as excinfo:
            await session.exec("sleep 5", timeout=1)

        assert excinfo.value.context["timeout_s"] == 1.0

    await client.delete(session)


async def test_timeout_error_reports_the_default_when_none_was_requested(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """Reporting None for something that just timed out would read as "no timeout"."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exec_timeout_default_s=1.0),
    )

    async with session:
        with pytest.raises(ExecTimeoutError) as excinfo:
            await session.exec("sleep 5", timeout=None)

        assert excinfo.value.context["timeout_s"] == 1.0

    await client.delete(session)
