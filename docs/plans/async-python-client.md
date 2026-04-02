# Async Python Client

## Context

The Python SDK (`k8s_agent_sandbox`) provides a synchronous client for creating
and interacting with Kubernetes-managed sandboxes. It uses `requests` for HTTP
and the synchronous `kubernetes` library for K8s API calls. Both block the event
loop, making the client unsuitable for async frameworks (FastAPI, aiohttp, async
agent orchestrators).

PR [#256](https://github.com/kubernetes-sigs/agent-sandbox/pull/256) by
@raceychan attempted to add async support but went stale. Key reviewer feedback:

1. **SHRUTI6991**: "A lot of this is duplicated code. Can you please refactor
   this class to use helper methods?" — the async client was a monolithic copy
   of the sync client.
2. **SHRUTI6991**: "Can we also add test cases for async client?"
3. The PR was written against the old `agentic_sandbox` package structure which
   has since been renamed to `k8s_agent_sandbox` and refactored into separate
   modules (connector, commands, files, sandbox, sandbox_client).

The codebase has been substantially restructured since PR #256. The current
modular architecture (separate `connector.py`, `commands/`, `files/`, `sandbox.py`,
`sandbox_client.py`) makes it natural to add async counterparts without
duplicating business logic.

### Current Architecture

```
k8s_agent_sandbox/
├── __init__.py              # Exports SandboxClient
├── sandbox_client.py        # SandboxClient — lifecycle management (create/get/delete)
├── sandbox.py               # Sandbox — connection handle with commands/files
├── connector.py             # SandboxConnector — HTTP via requests.Session
├── k8s_helper.py            # K8sHelper — sync K8s API calls
├── trace_manager.py         # OpenTelemetry tracing (already has mock fallback)
├── models.py                # Pydantic models (shared, no I/O)
├── exceptions.py            # Custom exceptions (shared, no I/O)
├── constants.py             # API group constants (shared)
├── commands/
│   └── command_executor.py  # CommandExecutor — sync command execution
└── files/
    └── filesystem.py        # Filesystem — sync file operations
```

## Goals

1. **Non-blocking sandbox operations** — users can `await` sandbox creation,
   command execution, and file I/O without blocking the event loop.
2. **API parity** — the async client exposes the same operations as the sync
   client: `create_sandbox`, `get_sandbox`, `delete_sandbox`, `delete_all`,
   `list_active_sandboxes`, `list_all_sandboxes`, plus `sandbox.commands.run()`,
   `sandbox.files.{write,read,list,exists}()`.
3. **Minimal code duplication** — shared logic (models, exceptions, constants,
   tracing setup) is reused; only I/O-bound code gets async counterparts.
4. **Optional dependencies** — `httpx` and `kubernetes_asyncio` are installed
   only when users opt in via `pip install k8s-agent-sandbox[async]`.
5. **Backward compatible** — zero changes to the sync client's public API.

## Non-Goals

- Async version of `LocalTunnelConnectionStrategy` (it shells out to `kubectl
  port-forward`, which is inherently sync and is only used for local dev).
- WebSocket or streaming command execution.
- Replacing the sync client — both coexist.

## Approach

### Step 1 — Add `async_trace_span` decorator to `trace_manager.py`

The existing `trace_span` decorator wraps sync methods. Add an
`async_trace_span` that wraps async methods with the same OpenTelemetry span
semantics.

- [ ] Add `async_trace_span(span_suffix)` decorator function after the existing
  `trace_span`.

### Step 2 — Create `async_k8s_helper.py`

Async equivalent of `k8s_helper.py` using `kubernetes_asyncio` instead of
`kubernetes`.

- [ ] Create `k8s_agent_sandbox/async_k8s_helper.py` with `AsyncK8sHelper`
  class.
- [ ] Mirror every method from `K8sHelper` as an async method.
- [ ] Use `kubernetes_asyncio.client`, `kubernetes_asyncio.config`, and
  `kubernetes_asyncio.watch`.
- [ ] Lazy initialization via `_ensure_initialized()` since
  `load_kube_config()` is async. Guard with `asyncio.Lock` to prevent
  concurrent initialization races.
- [ ] Use `async for event in w.stream(...)` instead of `for event in
  w.stream(...)`.
- [ ] Use `await w.close()` instead of `w.stop()`.
- [ ] Wrap all watch streams in `try/finally` to ensure `w.close()` is called
  even on exceptions or cancellation.
- [ ] Manage a shared `ApiClient` instance; expose `close()` method to shut it
  down cleanly.

### Step 3 — Create `async_connector.py`

Async equivalent of `connector.py` using `httpx.AsyncClient`.

- [ ] Create `k8s_agent_sandbox/async_connector.py` with
  `AsyncSandboxConnector` class.
- [ ] Support `SandboxDirectConnectionConfig` and
  `SandboxGatewayConnectionConfig` only (no LocalTunnel — document this
  limitation).
- [ ] Raise `ValueError` with a clear message if
  `SandboxLocalTunnelConnectionConfig` is passed.
- [ ] Implement explicit retry logic for HTTP status codes (500, 502, 503, 504)
  with exponential backoff, matching the sync client's
  `Retry(total=5, backoff_factor=0.5, status_forcelist=[500,502,503,504])`.
  `httpx.AsyncHTTPTransport(retries=N)` only retries on connection errors, not
  HTTP status codes — this must be implemented manually.
- [ ] Map `httpx.HTTPStatusError` → `SandboxRequestError` with status code.
- [ ] Map `httpx.HTTPError` → `SandboxRequestError` without status code.

### Step 4 — Update `exceptions.py`

- [ ] Change `SandboxRequestError.response` type annotation from
  `requests.Response | None` to `Any` so it accepts both `requests.Response`
  and `httpx.Response`.
- [ ] Remove the `import requests` at the top (replace with `from typing import Any`).

### Step 5 — Create `commands/async_command_executor.py`

- [ ] Create async version of `CommandExecutor`.
- [ ] Use `AsyncSandboxConnector` and `async_trace_span`.
- [ ] Same `run()` signature, returns `ExecutionResult`.

### Step 6 — Create `files/async_filesystem.py`

- [ ] Create async version of `Filesystem`.
- [ ] Use `AsyncSandboxConnector` and `async_trace_span`.
- [ ] Same method signatures: `write`, `read`, `list`, `exists`.

### Step 7 — Create `async_sandbox.py`

- [ ] Create `AsyncSandbox` class mirroring `Sandbox`.
- [ ] Wire up `AsyncSandboxConnector`, `AsyncCommandExecutor`,
  `AsyncFilesystem`.
- [ ] `get_pod_name()`, `_close_connection()`, `terminate()` are all async.
- [ ] Properties `commands`, `files`, `is_active` remain sync (they just return
  cached objects).
- [ ] Require explicit `connection_config` — no default. Raise `ValueError` if
  not provided, since LocalTunnel (the sync default) is not supported in async.

### Step 8 — Create `async_sandbox_client.py`

- [ ] Create `AsyncSandboxClient` class mirroring `SandboxClient`.
- [ ] Use `AsyncK8sHelper` for all K8s operations.
- [ ] `create_sandbox()`, `get_sandbox()`, `delete_sandbox()`, `delete_all()`,
  `list_all_sandboxes()` are all async.
- [ ] `list_active_sandboxes()` remains sync (no I/O, just reads dict).
- [ ] Label validation methods are static and shared unchanged.
- [ ] No `atexit` hook (async cleanup must be explicit via `await
  client.delete_all()` or `async with` pattern).
- [ ] Support async context manager (`__aenter__` / `__aexit__`).
- [ ] `__aexit__` calls `delete_all()` for safe cleanup.
- [ ] Require explicit `connection_config` — raise `ValueError` if not provided.
- [ ] Use `except BaseException` (not `except Exception`) in `create_sandbox`
  cleanup path to catch `asyncio.CancelledError`.
- [ ] Guard `_active_connection_sandboxes` mutations with `asyncio.Lock` to
  prevent races between concurrent coroutines at `await` points.

### Step 9 — Update `pyproject.toml`

- [ ] Add `async` optional dependency group: `httpx` and
  `kubernetes_asyncio`.
- [ ] Add `pytest-asyncio` to the `test` optional dependency group.

### Step 10 — Update `__init__.py`

- [ ] Conditionally export `AsyncSandboxClient` (only when async deps are
  installed, using a try/except block).

### Step 11 — Add unit tests

- [ ] Create `k8s_agent_sandbox/test/unit/test_async_sandboxclient.py`.
- [ ] Mirror test structure from `test_sandboxclient.py`.
- [ ] Test `AsyncSandboxClient` lifecycle: create, get, delete, delete_all.
- [ ] Test `AsyncSandboxConnector` with `httpx.MockTransport`.
- [ ] Test that `SandboxLocalTunnelConnectionConfig` raises `ValueError` in
  async connector.
- [ ] Test async context manager behavior.
- [ ] Test cancellation cleanup: verify claim is deleted when
  `CancelledError` fires mid-create.
- [ ] Guard all async tests with `pytest.importorskip("httpx")` /
  `pytest.importorskip("kubernetes_asyncio")` so they skip gracefully in CI
  environments that only install `.[test]`.

### Step 12 — Update README

- [ ] Add "Async Usage" section to README showing `AsyncSandboxClient` with
  `async with` and `await`.
- [ ] Document that LocalTunnel is not supported for async; use Direct or
  Gateway config.
- [ ] Document `pip install k8s-agent-sandbox[async]`.

## Files to Change

### Modified

| File | Reason |
|---|---|
| `k8s_agent_sandbox/trace_manager.py` | Add `async_trace_span` decorator |
| `k8s_agent_sandbox/exceptions.py` | Widen `SandboxRequestError.response` type to `Any` |
| `k8s_agent_sandbox/__init__.py` | Conditionally export `AsyncSandboxClient` |
| `pyproject.toml` | Add `async` and `test` optional deps |
| `README.md` | Add async usage documentation |

### New

| File | Reason |
|---|---|
| `k8s_agent_sandbox/async_k8s_helper.py` | Async K8s API wrapper |
| `k8s_agent_sandbox/async_connector.py` | Async HTTP connector using httpx |
| `k8s_agent_sandbox/commands/async_command_executor.py` | Async command execution |
| `k8s_agent_sandbox/files/async_filesystem.py` | Async file operations |
| `k8s_agent_sandbox/async_sandbox.py` | Async sandbox handle |
| `k8s_agent_sandbox/async_sandbox_client.py` | Async sandbox lifecycle client |
| `k8s_agent_sandbox/test/unit/test_async_sandboxclient.py` | Unit tests |
| `test_async_client.py` | Integration test script |

## Edge Cases

1. **Async deps not installed** — importing `AsyncSandboxClient` when `httpx`
   or `kubernetes_asyncio` are missing should raise `ImportError` with a
   message directing users to install `k8s-agent-sandbox[async]`.
2. **LocalTunnel config passed to async connector** — raises `ValueError`
   with an actionable error message (use Direct or Gateway config instead).
3. **K8s config not loaded** — `AsyncK8sHelper._ensure_initialized()` handles
   lazy loading with `asyncio.Lock`; `load_incluster_config()` is tried first
   (sync), then `load_kube_config()` (async).
4. **Concurrent coroutine access** — `_active_connection_sandboxes` dict
   mutations and `_ensure_initialized()` are guarded with `asyncio.Lock` to
   prevent interleaving at `await` points.
5. **Client not cleaned up** — without `atexit`, if the user forgets to call
   `delete_all()`, sandboxes leak. Mitigated by supporting `async with` context
   manager.
6. **httpx response vs requests response** — `SandboxRequestError.response`
   type is widened to `Any` so both work. Users who catch this exception and
   access `.response` need to know which type to expect based on which client
   they use.
7. **Task cancellation during create** — `asyncio.CancelledError` inherits from
   `BaseException`, not `Exception`. The `create_sandbox` cleanup path uses
   `except BaseException` to catch it and delete orphaned claims.
8. **Watch stream failures** — all watch loops are wrapped in `try/finally` to
   ensure `w.close()` is called, preventing leaked connections.
9. **Connection config required** — `AsyncSandboxClient` and `AsyncSandbox` do
   not provide a default `connection_config` because the sync default
   (`LocalTunnel`) is unsupported. Omitting it raises `ValueError` immediately.

## Testing Strategy

- **Unit tests** (`test_async_sandboxclient.py`): Mock `AsyncK8sHelper` and
  test `AsyncSandboxClient` lifecycle methods. Mirror existing
  `test_sandboxclient.py` structure.
- **Connector tests**: Use `httpx.MockTransport` to test
  `AsyncSandboxConnector.send_request` including error paths (status codes,
  connection errors).
- **Cancellation test**: Verify claim deletion when `CancelledError` fires
  during `create_sandbox`.
- **Import guard tests**: Verify graceful skip when async deps are missing.
- **Integration test** (`test_async_client.py`): Requires a running K8s
  cluster. Mirrors `test_client.py` but uses `asyncio.run()`.
- Use `pytest-asyncio` for async test functions.
- All async tests guarded with `pytest.importorskip()` for CI compatibility.

## Optional Enhancements

1. **Async context manager on `AsyncSandbox`** — `async with sandbox:` for
   auto-cleanup. (small effort)
2. **Connection pooling tuning** — expose httpx pool limits via config.
   (small effort)
3. **Streaming command output** — use httpx streaming responses for long-running
   commands. (medium effort, separate PR)

## Assumptions

- `kubernetes_asyncio` API mirrors `kubernetes` closely enough that the method
  signatures are compatible. Verified: both are auto-generated from the same
  OpenAPI spec.
- The project already uses Python 3.10+ (`requires-python = ">=3.10"`), so
  `async/await` syntax is fully supported.

## [P2] Notes (from plan review)

- `SandboxRequestError.response: Any` is a type-quality regression. A union
  type or protocol would be better, but `Any` is acceptable for the initial
  implementation since both response types share the same interface
  (`.json()`, `.status_code`, `.text`).
- `__aexit__ -> delete_all()` will delete sandboxes attached via `get_sandbox()`.
  This matches the sync client's `atexit` behavior. Ownership tracking can be
  added in a follow-up if needed.
- Testing strategy should include failure path tests for cancellation,
  concurrent access, and missing import UX. Added to the plan.
- README/docs updates added as a required deliverable.

## Open Questions

None — all questions were answered by codebase exploration and plan review.
