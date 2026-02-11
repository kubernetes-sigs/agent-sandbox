from types import SimpleNamespace
from unittest.mock import patch, MagicMock

import pytest

from langchain_agent_sandbox import (
    AgentSandboxBackend,
    SandboxPolicyWrapper,
    WarmPoolBackend,
    create_sandbox_backend_factory,
)


class StubClient:
    def __init__(self, run_result=None, read_bytes=b""):
        self.run_result = run_result or SimpleNamespace(stdout="", stderr="", exit_code=0)
        self.read_bytes = read_bytes
        self.last_command = None
        self.last_read_path = None
        self.requests = []

    def run(self, command, timeout=60):
        self.last_command = command
        return self.run_result

    def read(self, path, timeout=60):
        self.last_read_path = path
        return self.read_bytes

    def _request(self, method, endpoint, **kwargs):
        self.requests.append((method, endpoint, kwargs))
        return SimpleNamespace(status_code=200)


def _require(condition: bool, message: str) -> None:
    if not condition:
        pytest.fail(message)


def test_execute_combines_output_and_stderr():
    client = StubClient(run_result=SimpleNamespace(stdout="ok", stderr="err", exit_code=1))
    backend = AgentSandboxBackend(client)

    result = backend.execute("echo test")

    _require(result.output == "ok\nerr", "Unexpected combined output")
    _require(result.exit_code == 1, "Unexpected exit code")
    _require(result.truncated is False, "Unexpected truncation flag")


def test_read_missing_file_returns_error():
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: False

    response = backend.read("/missing.txt")

    _require(response == "Error: File '/missing.txt' not found", "Unexpected read error")


def test_write_existing_file_returns_error():
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    result = backend.write("/exists.txt", "data")

    _require(result.error is not None, "Expected write error")
    _require("already exists" in result.error, "Unexpected write error message")


def test_edit_multiple_occurrences_without_replace_all():
    client = StubClient(read_bytes=b"alpha beta alpha")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    result = backend.edit("/file.txt", "alpha", "gamma", replace_all=False)

    _require(result.error is not None, "Expected edit error")
    _require("appears multiple times" in result.error, "Unexpected edit error message")
    _require(result.occurrences == 2, "Unexpected occurrences count")


def test_to_internal_blocks_escape():
    client = StubClient()
    backend = AgentSandboxBackend(client)

    with pytest.raises(ValueError):
        backend._to_internal("../../etc/passwd")


def test_ls_info_parses_entries():
    client = StubClient(run_result=SimpleNamespace(stdout="file.txt\nsubdir/\n", stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)

    entries = backend.ls_info("/")

    _require(len(entries) == 2, "Unexpected number of entries")
    _require(entries[0]["path"] == "/file.txt", "Unexpected first entry path")
    _require(entries[0]["is_dir"] is False, "Unexpected first entry type")
    _require(entries[1]["path"] == "/subdir", "Unexpected second entry path")
    _require(entries[1]["is_dir"] is True, "Unexpected second entry type")


def test_upload_files_invalid_path():
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._to_internal = lambda _: (_ for _ in ()).throw(ValueError("escape"))

    responses = backend.upload_files({"/bad": b"payload"})

    _require(responses[0].error == "invalid_path", "Unexpected upload error code")


def test_download_files_missing():
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "missing"

    responses = backend.download_files(["/missing.txt"])

    _require(responses[0].error == "file_not_found", "Unexpected download error code")


def test_grep_raw_returns_matches():
    grep_output = "/app/test.py:10:def foo():\n/app/test.py:20:    foo()\n"
    client = StubClient(run_result=SimpleNamespace(stdout=grep_output, stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    matches = backend.grep_raw("foo", path="/")

    _require(isinstance(matches, list), "Expected list of matches")
    _require(len(matches) == 2, f"Expected 2 matches, got {len(matches)}")
    _require(matches[0]["path"] == "/test.py", f"Unexpected path: {matches[0]['path']}")
    _require(matches[0]["line"] == 10, f"Unexpected line: {matches[0]['line']}")
    _require(matches[0]["text"] == "def foo():", f"Unexpected text: {matches[0]['text']}")


def test_grep_raw_path_not_found():
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: False

    result = backend.grep_raw("pattern", path="/nonexistent")

    _require(isinstance(result, str), "Expected error string")
    _require("not found" in result, f"Unexpected error message: {result}")


def test_glob_info_returns_matching_files():
    find_output = "/app/src/main.py\n/app/src/utils.py\n/app/tests/test_main.py\n"
    client = StubClient(run_result=SimpleNamespace(stdout=find_output, stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)
    backend._is_dir = lambda _: False

    entries = backend.glob_info("*.py", path="/")

    _require(isinstance(entries, list), "Expected list of entries")
    _require(len(entries) == 3, f"Expected 3 entries, got {len(entries)}")


def test_glob_info_returns_empty_list_on_failure():
    client = StubClient(run_result=SimpleNamespace(stdout="", stderr="No such directory", exit_code=1))
    backend = AgentSandboxBackend(client)

    result = backend.glob_info("*.py", path="/nonexistent")

    _require(isinstance(result, list), "Expected list")
    _require(len(result) == 0, f"Expected empty list, got {result}")


def test_ls_info_returns_empty_list_on_failure():
    client = StubClient(run_result=SimpleNamespace(stdout="", stderr="No such directory", exit_code=2))
    backend = AgentSandboxBackend(client)

    result = backend.ls_info("/nonexistent")

    _require(isinstance(result, list), "Expected list")
    _require(len(result) == 0, f"Expected empty list, got {result}")


def test_edit_success_with_replace_all():
    client = StubClient(read_bytes=b"foo bar foo baz foo")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True
    backend._upload_bytes = lambda path, content: None

    result = backend.edit("/file.txt", "foo", "qux", replace_all=True)

    _require(result.error is None, f"Unexpected error: {result.error}")
    _require(result.occurrences == 3, f"Expected 3 occurrences, got {result.occurrences}")


def test_edit_success_single_occurrence():
    client = StubClient(read_bytes=b"hello world")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True
    backend._upload_bytes = lambda path, content: None

    result = backend.edit("/file.txt", "world", "universe", replace_all=False)

    _require(result.error is None, f"Unexpected error: {result.error}")
    _require(result.occurrences == 1, f"Expected 1 occurrence, got {result.occurrences}")


def test_to_internal_blocks_sibling_directory_escape():
    """Test that /appfoo doesn't match when root_dir=/app."""
    client = StubClient()
    backend = AgentSandboxBackend(client, root_dir="/app")

    # This path should fail because /appfoo is not under /app
    with pytest.raises(ValueError):
        backend._to_internal("/../appfoo/secret")


def test_to_internal_allows_root_dir_itself():
    client = StubClient()
    backend = AgentSandboxBackend(client, root_dir="/app")

    result = backend._to_internal("/")

    _require(result == "/app", f"Expected /app, got {result}")


def test_ensure_parent_dir_raises_on_failure():
    client = StubClient(run_result=SimpleNamespace(stdout="", stderr="Permission denied", exit_code=1))
    backend = AgentSandboxBackend(client)

    with pytest.raises(RuntimeError) as exc_info:
        backend._ensure_parent_dir("/app/nested/file.txt")

    _require("Cannot create parent directory" in str(exc_info.value), f"Unexpected error: {exc_info.value}")


# --- Factory pattern tests ---


def test_factory_pattern_creates_backend():
    """Test that create_sandbox_backend_factory returns a working factory."""
    with patch("langchain_agent_sandbox.backend.SandboxClient") as MockClient:
        mock_instance = MagicMock()
        MockClient.return_value = mock_instance

        factory = create_sandbox_backend_factory(
            template_name="test-template",
            namespace="test-ns",
            root_dir="/workspace",
        )

        # Factory should be callable
        _require(callable(factory), "Factory should be callable")

        # Call factory with a mock runtime
        mock_runtime = MagicMock()
        backend = factory(mock_runtime)

        # Should return an AgentSandboxBackend
        _require(isinstance(backend, AgentSandboxBackend), "Factory should return AgentSandboxBackend")

        # SandboxClient should have been called with the key args
        call_kwargs = MockClient.call_args.kwargs
        _require(call_kwargs["template_name"] == "test-template", "Expected template_name")
        _require(call_kwargs["namespace"] == "test-ns", "Expected namespace")
        # root_dir is passed to AgentSandboxBackend, not SandboxClient
        _require(backend._root_dir == "/workspace", f"Expected root_dir=/workspace, got {backend._root_dir}")


def test_factory_pattern_passes_kwargs():
    """Test that factory passes additional kwargs to from_template."""
    with patch("langchain_agent_sandbox.backend.SandboxClient") as MockClient:
        mock_instance = MagicMock()
        MockClient.return_value = mock_instance

        factory = create_sandbox_backend_factory(
            template_name="my-template",
            namespace="prod",
            gateway_name="my-gateway",
            server_port=9999,
        )

        factory(MagicMock())

        # Verify key kwargs were passed
        call_kwargs = MockClient.call_args.kwargs
        _require(call_kwargs["template_name"] == "my-template", "Expected template_name")
        _require(call_kwargs["namespace"] == "prod", "Expected namespace")
        _require(call_kwargs["gateway_name"] == "my-gateway", "Expected gateway_name")
        _require(call_kwargs["server_port"] == 9999, "Expected server_port")


# --- Policy wrapper tests ---


def test_policy_wrapper_blocks_denied_paths():
    """Test that write/edit are blocked on denied path prefixes."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    wrapped = SandboxPolicyWrapper(
        backend,
        deny_prefixes=["/etc", "/sys"],
    )

    # Write to denied path
    result = wrapped.write("/etc/passwd", "bad content")
    _require(result.error is not None, "Expected write to be denied")
    _require("Policy denied" in result.error, f"Unexpected error: {result.error}")

    # Edit on denied path
    result = wrapped.edit("/sys/kernel/config", "old", "new")
    _require(result.error is not None, "Expected edit to be denied")
    _require("Policy denied" in result.error, f"Unexpected error: {result.error}")


def test_policy_wrapper_blocks_denied_commands():
    """Test that execute is blocked on denied command patterns."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    wrapped = SandboxPolicyWrapper(
        backend,
        deny_commands=["rm -rf", "shutdown", "reboot"],
    )

    result = wrapped.execute("rm -rf /")
    _require(result.exit_code == 1, "Expected command to fail")
    _require("Policy denied" in result.output, f"Unexpected output: {result.output}")

    result = wrapped.execute("sudo shutdown now")
    _require(result.exit_code == 1, "Expected command to fail")
    _require("Policy denied" in result.output, f"Unexpected output: {result.output}")


def test_policy_wrapper_passes_allowed_operations():
    """Test that non-denied operations work through the wrapper."""
    client = StubClient(
        run_result=SimpleNamespace(stdout="ok", stderr="", exit_code=0),
        read_bytes=b"file content",
    )
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: False  # For write test
    backend._ensure_parent_dir = lambda _: None
    backend._upload_bytes = lambda path, content: None

    wrapped = SandboxPolicyWrapper(
        backend,
        deny_prefixes=["/etc"],
        deny_commands=["rm -rf"],
    )

    # Allowed command should work
    result = wrapped.execute("echo hello")
    _require(result.exit_code == 0, "Expected command to succeed")
    _require(result.output == "ok", f"Unexpected output: {result.output}")

    # Write to allowed path should work
    result = wrapped.write("/app/file.txt", "content")
    _require(result.error is None, f"Unexpected error: {result.error}")


def test_policy_wrapper_audit_log_called():
    """Test that audit callback is invoked for operations."""
    audit_calls = []

    def audit_log(operation: str, target: str, meta: dict):
        audit_calls.append((operation, target, meta))

    client = StubClient(run_result=SimpleNamespace(stdout="ok", stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: False
    backend._ensure_parent_dir = lambda _: None
    backend._upload_bytes = lambda path, content: None

    wrapped = SandboxPolicyWrapper(backend, audit_log=audit_log)

    # Execute should be logged
    wrapped.execute("echo test")
    _require(len(audit_calls) == 1, f"Expected 1 audit call, got {len(audit_calls)}")
    _require(audit_calls[0][0] == "execute", f"Expected execute, got {audit_calls[0][0]}")
    _require(audit_calls[0][1] == "echo test", f"Unexpected target: {audit_calls[0][1]}")

    # Write should be logged
    wrapped.write("/app/file.txt", "hello")
    _require(len(audit_calls) == 2, f"Expected 2 audit calls, got {len(audit_calls)}")
    _require(audit_calls[1][0] == "write", f"Expected write, got {audit_calls[1][0]}")
    _require(audit_calls[1][2]["size"] == 5, f"Expected size 5, got {audit_calls[1][2]}")


def test_policy_wrapper_upload_files_filters_denied():
    """Test that upload_files filters out denied paths."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "missing"
    backend._dir_state = lambda _: "writable"
    backend._upload_bytes = lambda path, content: None

    wrapped = SandboxPolicyWrapper(backend, deny_prefixes=["/etc"])

    responses = wrapped.upload_files({
        "/etc/passwd": b"bad",
        "/app/good.txt": b"good",
    })

    # Should have 2 responses
    _require(len(responses) == 2, f"Expected 2 responses, got {len(responses)}")

    # Find the denied one
    denied = [r for r in responses if r.path == "/etc/passwd"]
    _require(len(denied) == 1, "Expected denied response for /etc/passwd")
    _require(denied[0].error == "policy_denied", f"Expected policy_denied, got {denied[0].error}")


def test_policy_wrapper_read_operations_pass_through():
    """Test that read operations pass through without policy checks."""
    grep_output = "/app/test.py:10:def foo():\n"
    client = StubClient(
        run_result=SimpleNamespace(stdout=grep_output, stderr="", exit_code=0),
        read_bytes=b"content",
    )
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    # Even with very restrictive policies, reads should work
    wrapped = SandboxPolicyWrapper(
        backend,
        deny_prefixes=["/"],  # Would block everything if applied to reads
        deny_commands=["grep"],  # grep is used internally
    )

    # grep_raw should still work
    matches = wrapped.grep_raw("foo", path="/")
    _require(isinstance(matches, list), "Expected list of matches")


def test_policy_wrapper_context_manager():
    """Test that policy wrapper works as context manager."""
    # Use MagicMock for context manager support
    mock_client = MagicMock()
    mock_client.__enter__ = MagicMock(return_value=mock_client)
    mock_client.__exit__ = MagicMock(return_value=None)

    backend = AgentSandboxBackend(mock_client, manage_client=True)
    wrapped = SandboxPolicyWrapper(backend)

    with wrapped:
        mock_client.__enter__.assert_called_once()

    mock_client.__exit__.assert_called_once()


# --- WarmPool backend tests ---


def test_warmpool_backend_from_warmpool():
    """Test WarmPoolBackend.from_warmpool creates backend correctly."""
    with patch("langchain_agent_sandbox.backend.SandboxClient") as MockClient:
        mock_instance = MagicMock()
        MockClient.return_value = mock_instance

        backend = WarmPoolBackend.from_warmpool(
            template_name="fast-template",
            namespace="prod",
            warmpool_name="my-warmpool",
        )

        _require(isinstance(backend, WarmPoolBackend), "Should be WarmPoolBackend")
        _require(isinstance(backend, AgentSandboxBackend), "Should also be AgentSandboxBackend")

        MockClient.assert_called_once_with(
            template_name="fast-template",
            namespace="prod",
        )


def test_warmpool_backend_get_adoption_info():
    """Test get_adoption_info returns warmpool metadata."""
    with patch("langchain_agent_sandbox.backend.SandboxClient") as MockClient:
        mock_instance = MagicMock()
        MockClient.return_value = mock_instance

        # With warmpool name
        backend = WarmPoolBackend.from_warmpool(
            template_name="template",
            warmpool_name="pool-1",
        )
        info = backend.get_adoption_info()
        _require(info["warmpool_name"] == "pool-1", f"Expected pool-1, got {info['warmpool_name']}")
        _require(info["from_warmpool"] is True, "Expected from_warmpool=True")

        # Without warmpool name
        backend2 = WarmPoolBackend.from_warmpool(template_name="template")
        info2 = backend2.get_adoption_info()
        _require(info2["warmpool_name"] is None, f"Expected None, got {info2['warmpool_name']}")
        _require(info2["from_warmpool"] is False, "Expected from_warmpool=False")


def test_warmpool_backend_inherits_all_methods():
    """Test WarmPoolBackend inherits all AgentSandboxBackend methods."""
    client = StubClient(run_result=SimpleNamespace(stdout="ok", stderr="", exit_code=0))
    backend = WarmPoolBackend(client)

    # Should have all the standard methods
    _require(hasattr(backend, "execute"), "Should have execute")
    _require(hasattr(backend, "read"), "Should have read")
    _require(hasattr(backend, "write"), "Should have write")
    _require(hasattr(backend, "edit"), "Should have edit")
    _require(hasattr(backend, "ls_info"), "Should have ls_info")
    _require(hasattr(backend, "grep_raw"), "Should have grep_raw")
    _require(hasattr(backend, "glob_info"), "Should have glob_info")

    # Execute should work
    result = backend.execute("echo test")
    _require(result.output == "ok", f"Unexpected output: {result.output}")


# --- Additional coverage tests ---


def test_root_dir_must_be_absolute():
    """Test that root_dir validation rejects relative paths."""
    client = StubClient()

    with pytest.raises(ValueError) as exc_info:
        AgentSandboxBackend(client, root_dir="relative/path")

    _require("absolute path" in str(exc_info.value), f"Unexpected error: {exc_info.value}")


def test_upload_files_returns_upload_failed_on_exception():
    """Test that upload_files handles exceptions gracefully."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "missing"
    backend._dir_state = lambda _: "writable"
    backend._upload_bytes = lambda path, content: (_ for _ in ()).throw(RuntimeError("write failed"))

    responses = backend.upload_files({"/app/file.txt": b"data"})

    _require(len(responses) == 1, f"Expected 1 response, got {len(responses)}")
    _require(responses[0].error == "upload_failed", f"Expected upload_failed, got {responses[0].error}")


def test_download_files_returns_download_failed_on_exception():
    """Test that download_files handles exceptions gracefully."""
    client = StubClient()
    client.read = lambda path, timeout=60: (_ for _ in ()).throw(RuntimeError("read failed"))
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "file"

    responses = backend.download_files(["/app/file.txt"])

    _require(len(responses) == 1, f"Expected 1 response, got {len(responses)}")
    _require(responses[0].error == "download_failed", f"Expected download_failed, got {responses[0].error}")


@pytest.mark.asyncio
async def test_aexecute_delegates_to_execute():
    """Test that async execute delegates correctly."""
    client = StubClient(run_result=SimpleNamespace(stdout="async-ok", stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)

    result = await backend.aexecute("echo test")

    _require(result.output == "async-ok", f"Unexpected output: {result.output}")
    _require(result.exit_code == 0, f"Unexpected exit code: {result.exit_code}")


def test_read_with_offset_beyond_file_length():
    """Test read() with offset larger than file length."""
    client = StubClient(read_bytes=b"line1\nline2\nline3")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    result = backend.read("/file.txt", offset=100, limit=10)

    _require(result == "", f"Expected empty string, got: {result}")


def test_read_with_offset_and_limit():
    """Test read() pagination works correctly."""
    client = StubClient(read_bytes=b"line0\nline1\nline2\nline3\nline4")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    result = backend.read("/file.txt", offset=1, limit=2)

    # offset=1 means start at line index 1 (2nd line), get 2 lines
    # Output should show 1-based line numbers: "2: line1\n3: line2"
    _require("2: line1" in result, f"Expected line 2, got: {result}")
    _require("3: line2" in result, f"Expected line 3, got: {result}")
    _require("4: line3" not in result, f"Should not include line 4, got: {result}")


def test_edit_string_not_found_returns_error():
    """Test edit() when old_string is not found."""
    client = StubClient(read_bytes=b"hello world")
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    result = backend.edit("/file.txt", "missing", "replacement")

    _require(result.error is not None, "Expected error for missing string")
    _require("not found" in result.error, f"Unexpected error: {result.error}")
    _require(result.occurrences == 0, f"Expected 0 occurrences, got {result.occurrences}")


def test_id_property_returns_claim_name_when_available():
    """Test id property returns claim_name when available."""
    client = StubClient()
    client.claim_name = "my-claim"
    backend = AgentSandboxBackend(client)

    _require(backend.id == "my-claim", f"Expected my-claim, got {backend.id}")


def test_id_property_returns_sandbox_name_when_no_claim():
    """Test id property returns sandbox_name when no claim_name."""
    client = StubClient()
    client.sandbox_name = "my-sandbox"
    backend = AgentSandboxBackend(client)

    _require(backend.id == "my-sandbox", f"Expected my-sandbox, got {backend.id}")


def test_id_property_returns_default_when_no_names():
    """Test id property returns default when no names available."""
    client = StubClient()
    backend = AgentSandboxBackend(client)

    _require(backend.id == "agent-sandbox", f"Expected agent-sandbox, got {backend.id}")


def test_grep_raw_ignores_malformed_lines():
    """Test grep_raw handles malformed output lines gracefully."""
    # Mix of valid and malformed lines
    grep_output = "/app/file.py:10:valid match\nmalformed line without colons\n/app/file.py:invalid:line number\n"
    client = StubClient(run_result=SimpleNamespace(stdout=grep_output, stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: True

    matches = backend.grep_raw("valid", path="/")

    # Should only get the one valid match
    _require(isinstance(matches, list), "Expected list of matches")
    _require(len(matches) == 1, f"Expected 1 match, got {len(matches)}")
    _require(matches[0]["text"] == "valid match", f"Unexpected text: {matches[0]['text']}")


@pytest.mark.asyncio
async def test_policy_wrapper_async_write_enforces_policy():
    """Test that async write operations enforce policy."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    wrapped = SandboxPolicyWrapper(backend, deny_prefixes=["/etc"])

    result = await wrapped.awrite("/etc/passwd", "bad content")

    _require(result.error is not None, "Expected write to be denied")
    _require("Policy denied" in result.error, f"Unexpected error: {result.error}")


@pytest.mark.asyncio
async def test_policy_wrapper_async_execute_enforces_policy():
    """Test that async execute operations enforce policy."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    wrapped = SandboxPolicyWrapper(backend, deny_commands=["rm -rf"])

    result = await wrapped.aexecute("rm -rf /")

    _require(result.exit_code == 1, "Expected command to fail")
    _require("Policy denied" in result.output, f"Unexpected output: {result.output}")


# --- Additional tests for PR review coverage ---


def test_from_template_creates_managed_backend():
    """Test from_template creates backend with manage_client=True."""
    with patch("langchain_agent_sandbox.backend.SandboxClient") as MockClient:
        mock_instance = MagicMock()
        MockClient.return_value = mock_instance

        backend = AgentSandboxBackend.from_template(
            template_name="test-template",
            namespace="test-ns",
            gateway_name="my-gateway",
            root_dir="/workspace",
        )

        # Verify manage_client is True
        _require(backend._manage_client is True, "Expected manage_client=True")
        _require(backend._root_dir == "/workspace", f"Expected /workspace, got {backend._root_dir}")

        # Verify SandboxClient was called with correct args
        call_kwargs = MockClient.call_args.kwargs
        _require(call_kwargs["template_name"] == "test-template", "Expected template_name")
        _require(call_kwargs["namespace"] == "test-ns", "Expected namespace")
        _require(call_kwargs["gateway_name"] == "my-gateway", "Expected gateway_name")


def test_upload_bytes_raises_with_details():
    """Test _upload_bytes raises RuntimeError with stderr details."""
    client = StubClient(run_result=SimpleNamespace(stdout="", stderr="Permission denied", exit_code=1))
    backend = AgentSandboxBackend(client)

    with pytest.raises(RuntimeError) as exc_info:
        backend._upload_bytes("/app/file.txt", b"content")

    _require("Upload failed" in str(exc_info.value), f"Unexpected error: {exc_info.value}")
    _require("Permission denied" in str(exc_info.value), f"Expected stderr in error: {exc_info.value}")


def test_edit_nonexistent_file_returns_error():
    """Test edit() returns error when file doesn't exist."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._exists = lambda _: False

    result = backend.edit("/nonexistent.txt", "old", "new")

    _require(result.error is not None, "Expected error for non-existent file")
    _require("not found" in result.error, f"Unexpected error: {result.error}")
    _require(result.path == "/nonexistent.txt", f"Unexpected path: {result.path}")
    _require(result.occurrences == 0, f"Expected 0 occurrences, got {result.occurrences}")


def test_download_files_directory_returns_error():
    """Test download_files returns is_directory error for directories."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "dir"

    responses = backend.download_files(["/app/somedir"])

    _require(len(responses) == 1, f"Expected 1 response, got {len(responses)}")
    _require(responses[0].error == "is_directory", f"Expected is_directory, got {responses[0].error}")
    _require(responses[0].content is None, "Expected content to be None")


def test_upload_files_existing_directory_returns_error():
    """Test upload_files returns is_directory error when target is a directory."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "dir"

    responses = backend.upload_files({"/app/somedir": b"data"})

    _require(len(responses) == 1, f"Expected 1 response, got {len(responses)}")
    _require(responses[0].error == "is_directory", f"Expected is_directory, got {responses[0].error}")


def test_upload_files_permission_denied_file():
    """Test upload_files returns permission_denied for unreadable target."""
    client = StubClient()
    backend = AgentSandboxBackend(client)
    backend._file_state = lambda _: "denied"

    responses = backend.upload_files({"/app/restricted": b"data"})

    _require(len(responses) == 1, f"Expected 1 response, got {len(responses)}")
    _require(responses[0].error == "permission_denied", f"Expected permission_denied, got {responses[0].error}")


def test_audit_log_exception_does_not_block_operation():
    """Test that failing audit log callback doesn't prevent operation."""
    def failing_audit_log(operation: str, target: str, meta: dict):
        raise Exception("Audit service unavailable")

    client = StubClient(run_result=SimpleNamespace(stdout="ok", stderr="", exit_code=0))
    backend = AgentSandboxBackend(client)
    wrapped = SandboxPolicyWrapper(backend, audit_log=failing_audit_log)

    # Execute should still work despite audit log failure
    result = wrapped.execute("echo test")
    _require(result.exit_code == 0, "Expected command to succeed despite audit failure")
    _require(result.output == "ok", f"Unexpected output: {result.output}")
