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

"""Unit tests for bridge.py's WebMCPBridge.

WebMCPBridge's own logic — which JS snippet to run, what arguments to pass,
what to hand back — doesn't need a real browser to verify. These tests use
a fake Page exposing just the one method WebMCPBridge actually calls
(evaluate), so they run in milliseconds with no Playwright, no Chromium,
and no kind cluster.
"""

import asyncio

import pytest

import bridge


class FakePage:
    """Records evaluate() calls and returns a scripted response."""

    def __init__(self, response):
        self._response = response
        self.calls: list[tuple[str, object]] = []

    async def evaluate(self, script, arg=None):
        self.calls.append((script, arg))
        return self._response


def test_list_tools_returns_page_response():
    page = FakePage(response=[{"name": "get-server-time"}])
    wb = bridge.WebMCPBridge(page)

    result = asyncio.run(wb.list_tools())

    assert result == [{"name": "get-server-time"}]
    assert len(page.calls) == 1
    script, arg = page.calls[0]
    assert script == bridge._GET_TOOLS_JS
    assert arg is None


def test_execute_passes_tool_name_and_arguments_through():
    page = FakePage(response={"counter": 4})
    wb = bridge.WebMCPBridge(page)

    result = asyncio.run(wb.execute("increment-counter", {"by": 3}))

    assert result == {"counter": 4}
    assert len(page.calls) == 1
    script, arg = page.calls[0]
    assert script == bridge._EXECUTE_TOOL_JS
    assert arg == {"name": "increment-counter", "args": {"by": 3}}


def test_execute_with_empty_arguments():
    page = FakePage(response={"iso_time": "2026-01-01T00:00:00Z"})
    wb = bridge.WebMCPBridge(page)

    result = asyncio.run(wb.execute("get-server-time", {}))

    assert result == {"iso_time": "2026-01-01T00:00:00Z"}
    _, arg = page.calls[0]
    assert arg == {"name": "get-server-time", "args": {}}


def test_call_browser_tool_requires_started_bridge():
    bridge._bridge = None
    with pytest.raises(RuntimeError):
        asyncio.run(bridge.call_browser_tool.fn(tool_name="x", arguments={}))


def test_list_browser_tools_requires_started_bridge():
    bridge._bridge = None
    with pytest.raises(RuntimeError):
        asyncio.run(bridge.list_browser_tools.fn())


@pytest.fixture(autouse=True)
def reset_bridge_global():
    """Every test starts and ends with _bridge unset, so module-level
    state from one test can't leak into the next."""
    yield
    bridge._bridge = None


def test_list_browser_tools_delegates_to_started_bridge():
    page = FakePage(response=[{"name": "get-server-time"}])
    bridge._bridge = bridge.WebMCPBridge(page)

    result = asyncio.run(bridge.list_browser_tools.fn())

    assert result == [{"name": "get-server-time"}]


def test_call_browser_tool_delegates_to_started_bridge():
    page = FakePage(response={"counter": 4})
    bridge._bridge = bridge.WebMCPBridge(page)

    result = asyncio.run(
        bridge.call_browser_tool.fn(tool_name="increment-counter", arguments={"by": 3})
    )

    assert result == {"counter": 4}
    _, arg = page.calls[0]
    assert arg == {"name": "increment-counter", "args": {"by": 3}}


def test_execute_decodes_stringified_result():
    # The WebMCP spec defines executeTool() as returning a JSON-stringified
    # result; the bundled demo polyfill returns the handler's object
    # directly instead. Both must come out the same shape on the MCP side.
    page = FakePage(response='{"counter": 4}')
    wb = bridge.WebMCPBridge(page)

    result = asyncio.run(wb.execute("increment-counter", {"by": 3}))

    assert result == {"counter": 4}


def test_execute_returns_non_json_string_result_as_is():
    page = FakePage(response="plain text result")
    wb = bridge.WebMCPBridge(page)

    result = asyncio.run(wb.execute("some-tool", {}))

    assert result == "plain text result"


def test_execute_times_out_when_page_never_resolves():
    class HangingPage:
        async def evaluate(self, script, arg=None):
            await asyncio.sleep(3600)

    wb = bridge.WebMCPBridge(HangingPage())
    original_timeout = bridge._TOOL_CALL_TIMEOUT_S
    bridge._TOOL_CALL_TIMEOUT_S = 0.01
    try:
        with pytest.raises(RuntimeError):
            asyncio.run(wb.execute("slow-tool", {}))
    finally:
        bridge._TOOL_CALL_TIMEOUT_S = original_timeout


def test_list_tools_times_out_when_page_never_resolves():
    class HangingPage:
        async def evaluate(self, script, arg=None):
            await asyncio.sleep(3600)

    wb = bridge.WebMCPBridge(HangingPage())
    original_timeout = bridge._TOOL_CALL_TIMEOUT_S
    bridge._TOOL_CALL_TIMEOUT_S = 0.01
    try:
        with pytest.raises(RuntimeError):
            asyncio.run(wb.list_tools())
    finally:
        bridge._TOOL_CALL_TIMEOUT_S = original_timeout


def test_call_browser_tool_defaults_missing_arguments_to_empty_dict():
    page = FakePage(response={"iso_time": "2026-01-01T00:00:00Z"})
    bridge._bridge = bridge.WebMCPBridge(page)

    result = asyncio.run(bridge.call_browser_tool.fn(tool_name="get-server-time"))

    assert result == {"iso_time": "2026-01-01T00:00:00Z"}
    _, arg = page.calls[0]
    assert arg == {"name": "get-server-time", "args": {}}
