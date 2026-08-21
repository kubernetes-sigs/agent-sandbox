"""Fixtures shared by the provider test modules."""

from __future__ import annotations

from pathlib import Path

import pytest

from fake_k8s import FakeAsyncSandboxClient
from openai_agents_k8s_sandbox import K8sSandboxClient


@pytest.fixture
def fake_client(tmp_path: Path) -> FakeAsyncSandboxClient:
    return FakeAsyncSandboxClient(home=tmp_path / "home")


@pytest.fixture
def client(fake_client: FakeAsyncSandboxClient) -> K8sSandboxClient:
    return K8sSandboxClient(fake_client)


@pytest.fixture
def workspace(tmp_path: Path) -> Path:
    return tmp_path / "workspace"
