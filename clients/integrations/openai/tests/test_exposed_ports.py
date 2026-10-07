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

"""Exposed-port resolution.

The Sandbox CR gives the pod a stable in-cluster hostname, so there is no host-side port
remapping as with Docker: the container port *is* the published port. What varies is the
host, and getting it wrong points callers at something unreachable rather than failing.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from agents.sandbox.errors import ExposedPortUnavailableError
from agents.sandbox.manifest import Manifest

from openai_agents_k8s_sandbox import K8sSandboxClient
from support import NAMESPACE, make_options


async def test_resolves_to_the_in_cluster_dns_name(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exposed_ports=(8080, 9090)),
    )

    endpoint = await session.resolve_exposed_port(8080)

    assert endpoint.host == f"{session.state.sandbox_id}.{NAMESPACE}.svc.cluster.local"
    assert endpoint.port == 8080
    assert endpoint.tls is False

    await client.delete(session)


async def test_explicit_host_overrides_the_derived_one(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exposed_ports=(8080,), exposed_port_host="gateway.example.test"),
    )

    endpoint = await session.resolve_exposed_port(8080)
    assert endpoint.host == "gateway.example.test"

    await client.delete(session)


async def test_unconfigured_port_is_refused(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exposed_ports=(8080,)),
    )

    with pytest.raises(ExposedPortUnavailableError) as excinfo:
        await session.resolve_exposed_port(9999)

    assert excinfo.value.context["reason"] == "not_configured"

    await client.delete(session)


async def test_no_exposed_ports_means_no_host(
    client: K8sSandboxClient, workspace: Path
) -> None:
    session = await client.create(
        manifest=Manifest(root=str(workspace)), options=make_options()
    )

    assert session.state.exposed_port_host is None

    await client.delete(session)


async def test_configured_port_without_a_host_reports_backend_unavailable(
    client: K8sSandboxClient, workspace: Path
) -> None:
    """Reachable from a hand-edited or older persisted payload, where the host is unset."""

    session = await client.create(
        manifest=Manifest(root=str(workspace)),
        options=make_options(exposed_ports=(8080,)),
    )
    session.state.exposed_port_host = None

    with pytest.raises(ExposedPortUnavailableError) as excinfo:
        await session.resolve_exposed_port(8080)

    assert excinfo.value.context["reason"] == "backend_unavailable"

    await client.delete(session)
