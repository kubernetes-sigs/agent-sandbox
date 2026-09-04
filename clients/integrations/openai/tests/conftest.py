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
