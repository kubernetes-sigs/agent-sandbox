# Copyright 2025 The Kubernetes Authors.
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

from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest
from fastapi.testclient import TestClient

from sandbox_router import app

HEADERS = {"X-Sandbox-ID": "test-sb"}
METHODS = ["get", "post", "put", "delete", "patch"]
PATHS = [
    "/exec?foo=bar&n=1",
    "/exec",
    "/exec?a=1&b=2&c=3",
    "/exec?q=%E4%B8%AD%E6%96%87",
    pytest.param("/abc?", marks=pytest.mark.xfail(
        reason="trailing '?' is dropped when query string is empty; expected behavior"
    )),
]


@pytest.fixture()
def mock_proxy():
    """Replace the real httpx client so no network call is made."""
    async def _stream():
        yield b""

    resp = MagicMock(status_code=200, headers={}, aiter_bytes=_stream)
    mock = MagicMock()
    mock.build_request.return_value = httpx.Request("GET", "http://placeholder")
    mock.send = AsyncMock(return_value=resp)

    with patch("sandbox_router.client", mock):
        yield mock


def forwarded_url(mock) -> str:
    return mock.build_request.call_args.kwargs["url"]


@pytest.mark.parametrize("method", METHODS)
@pytest.mark.parametrize("path", PATHS)
def test_query_string_forwarding(mock_proxy, method, path):
    getattr(TestClient(app), method)(path, headers=HEADERS)
    assert forwarded_url(mock_proxy).endswith(path)
