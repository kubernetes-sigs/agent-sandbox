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

"""E2E tests for the langchain-agent-sandbox DeepAgents backend adapter.

Exercises every public method of AgentSandboxBackend against a real
sandbox pod running in a kind cluster. Tests are grouped by feature
area and run in a fixed order within each test function.

Required environment:
    LANGCHAIN_SANDBOX_TEMPLATE  - SandboxTemplate name (e.g. python-deepagent)
    LANGCHAIN_USE_TUNNEL=1      - or LANGCHAIN_API_URL / LANGCHAIN_GATEWAY_NAME

Optional:
    LANGCHAIN_NAMESPACE         - default: "default"
    LANGCHAIN_GATEWAY_NAMESPACE - default: same as LANGCHAIN_NAMESPACE
    LANGCHAIN_SERVER_PORT       - default: 8888
    LANGCHAIN_ROOT_DIR          - default: /app
"""

import os
from typing import Optional

import pytest

try:
    from langchain_agent_sandbox import (
        AgentSandboxBackend,
        SandboxPolicyWrapper,
    )
    from k8s_agent_sandbox import SandboxClient
    from k8s_agent_sandbox.models import (
        SandboxDirectConnectionConfig,
        SandboxGatewayConnectionConfig,
        SandboxLocalTunnelConnectionConfig,
    )
except ImportError:
    pytest.skip("langchain-agent-sandbox not installed", allow_module_level=True)


REQUIRED_TEMPLATE_ENV = "LANGCHAIN_SANDBOX_TEMPLATE"


def _get_env(name: str, default: Optional[str] = None) -> Optional[str]:
    value = os.environ.get(name, default)
    return value if value not in (None, "") else None


def _should_skip() -> Optional[str]:
    template_name = _get_env(REQUIRED_TEMPLATE_ENV)
    if not template_name:
        return f"Missing {REQUIRED_TEMPLATE_ENV} env var"

    api_url = _get_env("LANGCHAIN_API_URL")
    gateway_name = _get_env("LANGCHAIN_GATEWAY_NAME")
    allow_tunnel = _get_env("LANGCHAIN_USE_TUNNEL", "0") == "1"

    if not api_url and not gateway_name and not allow_tunnel:
        return (
            "Set LANGCHAIN_API_URL or LANGCHAIN_GATEWAY_NAME to connect, "
            "or LANGCHAIN_USE_TUNNEL=1 to allow kubectl port-forward"
        )
    return None


def _require(condition: bool, message: str) -> None:
    if not condition:
        pytest.fail(message)


def _build_client():
    """Build a SandboxClient from env vars."""
    api_url = _get_env("LANGCHAIN_API_URL")
    gateway_name = _get_env("LANGCHAIN_GATEWAY_NAME")
    gateway_namespace = _get_env(
        "LANGCHAIN_GATEWAY_NAMESPACE",
        _get_env("LANGCHAIN_NAMESPACE", "default"),
    )
    server_port = int(_get_env("LANGCHAIN_SERVER_PORT", "8888"))

    if api_url:
        connection_config = SandboxDirectConnectionConfig(
            api_url=api_url, server_port=server_port,
        )
    elif gateway_name:
        connection_config = SandboxGatewayConnectionConfig(
            gateway_name=gateway_name,
            gateway_namespace=gateway_namespace,
            server_port=server_port,
        )
    else:
        connection_config = SandboxLocalTunnelConnectionConfig(
            server_port=server_port,
        )
    return SandboxClient(connection_config=connection_config)


def _template_name():
    return _get_env(REQUIRED_TEMPLATE_ENV)


def _namespace():
    return _get_env("LANGCHAIN_NAMESPACE", "default")


def _root_dir():
    return _get_env("LANGCHAIN_ROOT_DIR", "/app")


# ── Core protocol: execute, ls, read, write, edit, grep, glob ──


@pytest.mark.e2e
def test_execute_basic():
    """execute() runs a command and returns combined output."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        result = backend.execute("echo 'hello from deepagents'")
        _require("hello from deepagents" in result.output, f"Unexpected output: {result.output}")
        _require(result.exit_code == 0, f"Expected exit_code=0, got {result.exit_code}")
        _require(result.truncated is False, "Unexpected truncation")


@pytest.mark.e2e
def test_execute_nonzero_exit_code():
    """execute() returns non-zero exit code for failing commands."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        result = backend.execute("exit 42")
        _require(result.exit_code == 42, f"Expected exit_code=42, got {result.exit_code}")


@pytest.mark.e2e
def test_execute_cwd_is_root_dir():
    """execute() should run commands with cwd set to root_dir."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    root = _root_dir()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=root,
    ) as backend:
        result = backend.execute("pwd")
        _require(
            result.output.strip() == root,
            f"Expected cwd={root}, got: {result.output.strip()}",
        )


@pytest.mark.e2e
def test_execute_with_timeout():
    """execute(timeout=N) should pass timeout to the sandbox."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        # Short command with explicit timeout should succeed
        result = backend.execute("echo fast", timeout=30)
        _require(result.exit_code == 0, f"Expected exit_code=0, got {result.exit_code}")


@pytest.mark.e2e
def test_ls_lists_directory():
    """ls() returns structured directory entries with metadata."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        # Create a file so ls has something to list
        backend.execute("echo content > ls_test.txt && mkdir -p ls_subdir")

        ls_result = backend.ls("/")
        _require(ls_result.error is None, f"ls failed: {ls_result.error}")
        _require(len(ls_result.entries) > 0, "ls returned no entries")

        # Find our file
        file_entry = next(
            (e for e in ls_result.entries if e["path"].endswith("ls_test.txt")),
            None,
        )
        _require(file_entry is not None, "ls_test.txt not found in ls output")
        _require(file_entry["is_dir"] is False, "ls_test.txt should not be a directory")

        # Find our directory
        dir_entry = next(
            (e for e in ls_result.entries if e["path"].endswith("ls_subdir")),
            None,
        )
        _require(dir_entry is not None, "ls_subdir not found in ls output")
        _require(dir_entry["is_dir"] is True, "ls_subdir should be a directory")

        # Check metadata fields are populated
        if file_entry.get("size") is not None:
            _require(file_entry["size"] > 0, "File size should be > 0")
        if file_entry.get("modified_at") is not None:
            _require(len(file_entry["modified_at"]) > 0, "modified_at should be non-empty")


@pytest.mark.e2e
def test_write_read_edit_lifecycle():
    """Full write → read → edit → read lifecycle."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        # Write
        file_path = "/lifecycle_test.txt"
        write_res = backend.write(file_path, "line1\nline2\nline3\n")
        _require(write_res.error is None, f"Write failed: {write_res.error}")

        # Write again should fail (file exists)
        write_dup = backend.write(file_path, "duplicate")
        _require(write_dup.error is not None, "Write to existing file should fail")
        _require("already exists" in write_dup.error, f"Unexpected error: {write_dup.error}")

        # Read full
        read_res = backend.read(file_path)
        _require(read_res.error is None, f"Read failed: {read_res.error}")
        _require("line1" in read_res.file_data["content"], "Missing line1")
        _require("line3" in read_res.file_data["content"], "Missing line3")

        # Read with offset and limit
        read_slice = backend.read(file_path, offset=1, limit=1)
        _require(read_slice.error is None, f"Read slice failed: {read_slice.error}")
        _require(
            "line2" in read_slice.file_data["content"],
            f"Expected line2 at offset=1, got: {read_slice.file_data['content']}",
        )

        # Edit single occurrence
        edit_res = backend.edit(file_path, "line2", "REPLACED")
        _require(edit_res.error is None, f"Edit failed: {edit_res.error}")
        _require(edit_res.occurrences == 1, f"Expected 1 occurrence, got {edit_res.occurrences}")

        # Verify edit
        read_after = backend.read(file_path)
        _require("REPLACED" in read_after.file_data["content"], "Edit didn't take effect")
        _require("line2" not in read_after.file_data["content"], "Old text still present")

        # Edit with replace_all
        edit_all = backend.edit(file_path, "line", "LINE", replace_all=True)
        _require(edit_all.error is None, f"Edit all failed: {edit_all.error}")
        _require(edit_all.occurrences == 2, f"Expected 2 occurrences, got {edit_all.occurrences}")


@pytest.mark.e2e
def test_grep_basic_and_with_glob():
    """grep() finds matches, with optional glob filter."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        backend.write("/grep_target.py", "# grep marker alpha\nprint('hello')\n")
        backend.write("/grep_other.txt", "# grep marker alpha in txt\n")

        # Basic grep
        grep_res = backend.grep("grep marker alpha", path="/")
        _require(grep_res.error is None, f"Grep failed: {grep_res.error}")
        _require(len(grep_res.matches) >= 2, f"Expected >=2 matches, got {len(grep_res.matches)}")

        # Grep with glob filter — only .py files
        grep_py = backend.grep("grep marker alpha", path="/", glob="*.py")
        _require(grep_py.error is None, f"Grep with glob failed: {grep_py.error}")
        _require(
            all(m["path"].endswith(".py") for m in grep_py.matches),
            f"Glob filter didn't work: {[m['path'] for m in grep_py.matches]}",
        )

        # Grep no match
        grep_none = backend.grep("zzz_no_match_zzz", path="/")
        _require(grep_none.error is None, "No-match grep should not error")
        _require(len(grep_none.matches) == 0, "Expected 0 matches for non-existent pattern")


@pytest.mark.e2e
def test_glob_with_metadata():
    """glob() finds files by pattern and includes size/modified_at."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        backend.write("/glob_test.py", "print('hello')\n")
        backend.execute("mkdir -p subdir && echo data > subdir/nested.py")

        glob_res = backend.glob("**/*.py", path="/")
        _require(glob_res.error is None, f"Glob failed: {glob_res.error}")
        _require(len(glob_res.matches) >= 1, f"Expected >=1 match, got {len(glob_res.matches)}")

        # Verify at least one match has metadata
        match = glob_res.matches[0]
        _require("path" in match, "Match missing 'path'")
        _require("is_dir" in match, "Match missing 'is_dir'")
        # size and modified_at are optional but should be populated by our implementation
        if match.get("size") is not None:
            _require(isinstance(match["size"], int), f"size should be int, got {type(match['size'])}")
        if match.get("modified_at") is not None:
            _require(len(match["modified_at"]) > 0, "modified_at should be non-empty")


# ── File transfer: upload_files, download_files ──


@pytest.mark.e2e
def test_upload_download_roundtrip():
    """upload_files + download_files roundtrip with multiple files."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        files = [
            ("/upload_a.bin", b"\x00\x01\x02\x03"),
            ("/nested/upload_b.txt", b"hello upload"),
        ]
        upload_res = backend.upload_files(files)
        _require(len(upload_res) == 2, f"Expected 2 responses, got {len(upload_res)}")
        for i, resp in enumerate(upload_res):
            _require(resp.error is None, f"Upload [{i}] failed: {resp.error}")

        download_res = backend.download_files(["/upload_a.bin", "/nested/upload_b.txt"])
        _require(len(download_res) == 2, f"Expected 2 download responses, got {len(download_res)}")
        _require(download_res[0].content == b"\x00\x01\x02\x03", "Binary roundtrip failed")
        _require(download_res[1].content == b"hello upload", "Text roundtrip failed")


@pytest.mark.e2e
def test_download_missing_file():
    """download_files returns file_not_found for non-existent files."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        downloads = backend.download_files(["/does_not_exist_xyz.txt"])
        _require(downloads[0].error is not None, "Expected error for missing file")
        _require(downloads[0].content is None, "Content should be None for missing file")


# ── Lifecycle: from_existing, id, session_id ──


@pytest.mark.e2e
def test_from_existing_unmanaged():
    """from_existing() wraps a sandbox without managing lifecycle."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    sandbox = client.create_sandbox(
        template=_template_name(), namespace=_namespace(),
    )
    try:
        backend = AgentSandboxBackend.from_existing(sandbox, root_dir=_root_dir())
        result = backend.execute("echo from_existing works")
        _require(
            "from_existing works" in result.output,
            f"Unexpected output: {result.output}",
        )
        _require(result.exit_code == 0, f"Unexpected exit code: {result.exit_code}")
    finally:
        client.delete_sandbox(
            claim_name=sandbox.claim_name, namespace=sandbox.namespace,
        )


@pytest.mark.e2e
def test_id_is_namespace_qualified():
    """backend.id returns {namespace}/{claim_name}."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        backend_id = backend.id
        _require("/" in backend_id, f"id should be namespace-qualified, got: {backend_id}")
        ns, claim = backend_id.split("/", 1)
        _require(ns == _namespace(), f"Expected namespace={_namespace()}, got {ns}")
        _require(len(claim) > 0, "Claim name should be non-empty")


@pytest.mark.e2e
def test_session_reattach():
    """session_id enables sandbox reuse across invocations."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    import uuid
    session_id = f"e2e-test-{uuid.uuid4().hex[:8]}"
    client = _build_client()

    # First invocation: create sandbox with session label, write state
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
        session_id=session_id,
    ) as backend:
        first_id = backend.id
        backend.execute("echo 'persisted state' > /app/session_state.txt")
        # On exit: detaches without deleting (session sandbox persists)

    # Second invocation: reattach to the same sandbox
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
        session_id=session_id,
    ) as backend:
        _require(backend._reattached is True, "Should have reattached")
        _require(backend.id == first_id, f"Should reuse same sandbox: {backend.id} != {first_id}")

        # Verify state persisted
        read_res = backend.read("/session_state.txt")
        _require(read_res.error is None, f"Read failed: {read_res.error}")
        _require(
            "persisted state" in read_res.file_data["content"],
            f"State didn't persist: {read_res.file_data['content']}",
        )

    # Cleanup: delete the session sandbox
    AgentSandboxBackend.delete_all(
        client, namespace=_namespace(),
        label_selector=f"agent-sandbox.sigs.k8s.io/session-id={session_id}",
    )


# ── delete_all ──


@pytest.mark.e2e
def test_delete_all_with_label_selector():
    """delete_all(label_selector=...) only deletes matching claims.

    Creates a labeled SandboxClaim via the SDK directly (without the
    backend context manager, whose ``__exit__`` would delete it first)
    and then verifies ``delete_all`` removes exactly the labeled claim.
    """
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    import uuid
    label_value = f"e2e-cleanup-{uuid.uuid4().hex[:8]}"
    label_selector = f"e2e-test={label_value}"
    client = _build_client()

    # Create a labeled claim directly — no context manager means nothing
    # else will try to tear it down before delete_all runs.
    sandbox = client.create_sandbox(
        template=_template_name(),
        namespace=_namespace(),
        labels={"e2e-test": label_value},
    )
    try:
        matching_before = client.list_all_sandboxes(
            namespace=_namespace(), label_selector=label_selector,
        )
        _require(
            sandbox.claim_name in matching_before,
            f"Labeled claim '{sandbox.claim_name}' not found via selector; "
            f"selector returned {matching_before}",
        )
    finally:
        # Always drop our local handle to the port-forward / tunnel before
        # delete_all runs, so the kubelet delete isn't racing a live conn.
        try:
            sandbox.close_connection()
        except Exception:
            pass

    deleted = AgentSandboxBackend.delete_all(
        client, namespace=_namespace(),
        label_selector=label_selector,
    )
    _require(deleted >= 1, f"Expected to delete >=1 claim, deleted {deleted}")

    matching_after = client.list_all_sandboxes(
        namespace=_namespace(), label_selector=label_selector,
    )
    _require(
        not matching_after,
        f"Selector still matches after delete_all: {matching_after}",
    )


# ── Policy wrapper ──


@pytest.mark.e2e
def test_policy_wrapper_blocks_denied_command():
    """SandboxPolicyWrapper blocks denied commands."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        wrapper = SandboxPolicyWrapper(
            backend,
            deny_commands=["rm -rf"],
        )

        # Allowed command
        result = wrapper.execute("echo allowed")
        _require(result.exit_code == 0, "Allowed command should succeed")

        # Denied command
        result = wrapper.execute("rm -rf /tmp/something")
        _require(
            "Policy denied" in result.output,
            f"Expected policy denial, got: {result.output}",
        )
        _require(result.exit_code == 1, "Denied command should return exit_code=1")

        # Read ops pass through
        ls_res = wrapper.ls("/")
        _require(ls_res.error is None, "ls should pass through policy wrapper")


@pytest.mark.e2e
def test_policy_wrapper_blocks_denied_path():
    """SandboxPolicyWrapper blocks writes to denied paths."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
        allow_absolute_paths=True,
    ) as backend:
        wrapper = SandboxPolicyWrapper(
            backend,
            deny_prefixes=["/etc"],
        )

        # Denied write
        result = wrapper.write("/etc/should_fail.txt", "nope")
        _require(result.error is not None, "Write to /etc should be denied")
        _require("Policy denied" in result.error, f"Unexpected error: {result.error}")


# ── Simple skill test (agent invoke) ──


@pytest.mark.e2e
def test_agent_invoke_simple_skill():
    """A DeepAgent with sandbox backend can execute a simple task."""
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    # This test requires an LLM API key
    has_api_key = any(
        os.environ.get(key) for key in [
            "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY",
        ]
    )
    if not has_api_key:
        pytest.skip("No LLM API key set (ANTHROPIC_API_KEY, OPENAI_API_KEY, or GOOGLE_API_KEY)")

    try:
        from deepagents import create_deep_agent
    except ImportError:
        pytest.skip("deepagents not installed")

    # Get an LLM model
    model = None
    if os.environ.get("ANTHROPIC_API_KEY"):
        from langchain_anthropic import ChatAnthropic
        model = ChatAnthropic(model="claude-sonnet-4-20250514")
    elif os.environ.get("OPENAI_API_KEY"):
        from langchain_openai import ChatOpenAI
        model = ChatOpenAI(model="gpt-4o")
    elif os.environ.get("GOOGLE_API_KEY"):
        from langchain_google_genai import ChatGoogleGenerativeAI
        model = ChatGoogleGenerativeAI(model="gemini-1.5-pro")

    client = _build_client()
    with AgentSandboxBackend.from_template(
        client, template_name=_template_name(),
        namespace=_namespace(), root_dir=_root_dir(),
    ) as backend:
        agent = create_deep_agent(model=model, backend=backend)
        result = agent.invoke(
            "Create a file called /app/skill_test.txt containing exactly "
            "'skill test passed' and nothing else."
        )

        # Verify the agent created the file
        read_res = backend.read("/skill_test.txt")
        _require(read_res.error is None, f"Read failed: {read_res.error}")
        _require(
            "skill test passed" in read_res.file_data["content"],
            f"Agent didn't create expected file content: {read_res.file_data['content']}",
        )
