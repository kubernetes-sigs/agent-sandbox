#!/usr/bin/env python3
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

"""Bridge a WebMCP-enabled page's document.modelContext tools onto MCP.

WebMCP (https://github.com/webmachinelearning/webmcp) is a browser-side
proposal: a page registers tools — name, description, JSON schema, and a
handler — on document.modelContext, and an agent discovers/invokes them via
getTools()/executeTool(). Those tools only exist inside a live browser that
has actually loaded the page; there is no way for a backend service to host
them directly. This bridge runs inside the Sandbox pod, drives a real
headless browser to the target page via Playwright, and re-exposes whatever
tools that page registered as two MCP tools any MCP client can call.

Runs both Playwright and FastMCP inside the same asyncio event loop
(_main(), invoked via one asyncio.run()) rather than starting Playwright in
one asyncio.run() and handing off to mcp.run()'s own loop separately —
Playwright's async Page/Browser objects are bound to the loop that created
them, and calling into a Page from a second loop hangs rather than raising.
"""
import asyncio
import json
import logging
import os
from typing import Any

from fastmcp import FastMCP
from playwright.async_api import Page, TimeoutError as PlaywrightTimeoutError, async_playwright

logger = logging.getLogger(__name__)

TARGET_PAGE_URL = os.environ.get("TARGET_PAGE_URL", "http://localhost:8090/")

# Bounds how long a single WebMCP tool call may run before this bridge gives
# up on it and reports a timeout to the MCP client, rather than leaving the
# call hanging forever if the page's handler never resolves its promise.
# Timing out here does not stop whatever side effect the handler already
# started in the page — only the bridge's own wait for its result.
_TOOL_CALL_TIMEOUT_S = 30

_GET_TOOLS_JS = """
async () => (await document.modelContext.getTools())
  .map(({ name, description, inputSchema }) => ({ name, description, inputSchema }))
"""

_EXECUTE_TOOL_JS = """
async ({ name, args }) => {
  const tools = await document.modelContext.getTools();
  const tool = tools.find((t) => t.name === name);
  if (!tool) throw new Error(`unknown WebMCP tool: ${name}`);
  return await document.modelContext.executeTool(tool, args);
}
"""


class WebMCPBridge:
    def __init__(self, page: Page):
        self._page = page

    async def list_tools(self) -> list[dict]:
        try:
            return await asyncio.wait_for(
                self._page.evaluate(_GET_TOOLS_JS),
                timeout=_TOOL_CALL_TIMEOUT_S,
            )
        except asyncio.TimeoutError:
            raise RuntimeError(
                f"getTools() on {TARGET_PAGE_URL} did not respond within "
                f"{_TOOL_CALL_TIMEOUT_S}s"
            ) from None

    async def execute(self, tool_name: str, arguments: dict[str, Any]) -> Any:
        # A WebMCP tool's execute() can return anything JSON-serializable
        # per its own declared schema, not just a dict — Any is accurate
        # here, unlike the tool-listing shape above.
        try:
            result = await asyncio.wait_for(
                self._page.evaluate(
                    _EXECUTE_TOOL_JS, {"name": tool_name, "args": arguments}
                ),
                timeout=_TOOL_CALL_TIMEOUT_S,
            )
        except asyncio.TimeoutError:
            raise RuntimeError(
                f"WebMCP tool {tool_name!r} did not respond within "
                f"{_TOOL_CALL_TIMEOUT_S}s — any side effect it already started "
                "in the page may still be running"
            ) from None
        # The WebMCP spec defines executeTool() as returning a JSON-stringified
        # result; this example's demo polyfill instead hands back the
        # handler's object directly, which is also legal per the spec's own
        # "or an equivalent in-memory value" wording but not what a real
        # implementation returns. Decode string results so both shapes come
        # out the same on the MCP side — a real WebMCP page and the bundled
        # demo page behave identically to whatever calls this bridge.
        if isinstance(result, str):
            try:
                return json.loads(result)
            except ValueError:
                return result
        return result


mcp = FastMCP("WebMCP Bridge")
_bridge: WebMCPBridge | None = None


def _require_bridge() -> WebMCPBridge:
    # Not an assert: asserts are stripped when Python runs under -O, which
    # would turn "bridge not started" into a confusing AttributeError deep
    # inside WebMCPBridge instead of a clear error at the call site.
    if _bridge is None:
        raise RuntimeError("bridge not started")
    return _bridge


@mcp.tool()
async def list_browser_tools() -> list[dict]:
    """List WebMCP tools currently registered by the bridged page."""
    return await _require_bridge().list_tools()


@mcp.tool()
async def call_browser_tool(tool_name: str, arguments: dict[str, Any] | None = None) -> Any:
    """Invoke a WebMCP tool registered by the bridged page.

    Return type is Any, not dict, matching WebMCPBridge.execute() — a
    WebMCP tool can return anything JSON-serializable per its own
    declared schema. Both bundled demo tools happen to return a dict
    today, which would pass a -> dict annotation too, but that would
    silently contradict the "any WebMCP-enabled page" design goal this
    bridge is supposed to stay generic across.

    arguments defaults to None (turned into {} below), not {} directly —
    a mutable default argument would be shared across every call that
    omits it, which happens to be harmless here since it's never mutated,
    but isn't worth relying on staying that way.

    This example calls the page's tool directly with no policy check in
    front of it — see the README's "Governing tool calls" section for why
    a real deployment should classify each tool_name (read vs. mutating,
    at minimum) and gate mutating calls before they reach the page, the
    same way you would gate an agent-initiated shell command.
    """
    return await _require_bridge().execute(tool_name, arguments or {})


async def _start_bridge() -> None:
    global _bridge
    # No matching shutdown/close of browser or playwright here: this
    # example pod is single-shot (one page, lives until the pod does), so
    # the OS reclaims everything on exit. A long-running service adapted
    # from this would need to track both and close them (browser.close(),
    # then playwright.stop()) on shutdown to avoid leaking the browser
    # process across restarts/reconnects.
    playwright = await async_playwright().start()
    # chromium_sandbox defaults to False in Playwright's own API. It stays
    # False here rather than being forced True: Chromium's sandbox needs
    # unprivileged user namespaces, which most container runtimes (plain
    # `docker run`, and most Kubernetes clusters without an explicit seccomp
    # profile for it) don't grant — turning this on breaks the example's
    # basic quick-start on those setups instead of just hardening it. A
    # deployment that bridges an untrusted TARGET_PAGE_URL and has that
    # runtime support in place should set chromium_sandbox=True here; see
    # the README's "Governing tool calls"/hardening notes.
    browser = await playwright.chromium.launch()
    page = await browser.new_page()
    await page.goto(TARGET_PAGE_URL)
    # Explicit timeout, not Playwright's 30s default: a misconfigured
    # TARGET_PAGE_URL (wrong port, page never installs the polyfill) should
    # fail loudly and quickly, not hang silently for half a minute before
    # producing any output.
    try:
        await page.wait_for_function("() => !!document.modelContext", timeout=10000)
    except PlaywrightTimeoutError:
        raise RuntimeError(
            f"document.modelContext never appeared on {TARGET_PAGE_URL} after 10s — "
            "check TARGET_PAGE_URL is correct and the page installs the WebMCP "
            "polyfill (see demo-page/index.html) or ships the real API"
        ) from None
    # document.modelContext existing only means the polyfill/API installed —
    # a page that registers tools asynchronously (e.g. after its own fetch
    # or a framework mount) can still have zero tools at this point. Wait
    # for at least one, with a bounded timeout so a page that legitimately
    # never registers any tool doesn't hang startup forever.
    try:
        await page.wait_for_function(
            "async () => (await document.modelContext.getTools()).length > 0",
            timeout=5000,
        )
    except PlaywrightTimeoutError:
        logger.warning(
            "no WebMCP tools registered on %s after 5s — proceeding anyway; "
            "list_browser_tools will keep re-checking on every call",
            TARGET_PAGE_URL,
        )
    _bridge = WebMCPBridge(page)


async def _main() -> None:
    await _start_bridge()
    await mcp.run_async()


if __name__ == "__main__":
    asyncio.run(_main())
