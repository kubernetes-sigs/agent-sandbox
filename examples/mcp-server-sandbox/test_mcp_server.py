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

"""Unit tests for mcp_server.py.

mcp_server.py resolves its WORKSPACE directory from the MCP_WORKSPACE
environment variable at import time, so it must be pointed at a scratch
directory before the module is imported.
"""

import hashlib
import os
import shutil
import tempfile

import pytest

_workspace_dir = tempfile.mkdtemp(prefix="mcp-server-test-")
os.environ["MCP_WORKSPACE"] = _workspace_dir

import mcp_server  # noqa: E402  (must be imported after MCP_WORKSPACE is set)


@pytest.fixture(autouse=True)
def clean_workspace():
    """Start each test with an empty workspace directory."""
    yield
    for entry in os.listdir(_workspace_dir):
        path = os.path.join(_workspace_dir, entry)
        if os.path.isdir(path):
            shutil.rmtree(path)
        else:
            os.remove(path)


def test_list_blobs_empty_workspace():
    assert mcp_server.list_blobs() == []


def test_list_blobs_returns_sorted_filenames_only():
    (mcp_server.WORKSPACE / "b.txt").write_bytes(b"b")
    (mcp_server.WORKSPACE / "a.txt").write_bytes(b"a")
    (mcp_server.WORKSPACE / "subdir").mkdir()

    assert mcp_server.list_blobs() == ["a.txt", "b.txt"]


def test_write_random_blob_writes_expected_size_and_hash():
    result = mcp_server.write_random_blob("foo.bin", 128)

    written_path = mcp_server.WORKSPACE / "foo.bin"
    data = written_path.read_bytes()
    assert len(data) == 128
    assert result["path"] == str(written_path)
    assert result["bytes_written"] == 128
    assert result["sha256"] == hashlib.sha256(data).hexdigest()


def test_write_random_blob_zero_bytes_is_allowed():
    result = mcp_server.write_random_blob("empty.bin", 0)

    assert result["bytes_written"] == 0
    assert (mcp_server.WORKSPACE / "empty.bin").read_bytes() == b""


def test_write_random_blob_rejects_negative_size():
    with pytest.raises(ValueError):
        mcp_server.write_random_blob("x.bin", -1)


def test_write_random_blob_rejects_oversized():
    with pytest.raises(ValueError):
        mcp_server.write_random_blob("x.bin", 16 * 1024 * 1024 + 1)


def test_write_random_blob_rejects_path_traversal():
    with pytest.raises(ValueError):
        mcp_server.write_random_blob("../escape.bin", 8)


def test_read_blob_returns_size_and_hash():
    mcp_server.write_random_blob("data.bin", 64)
    data = (mcp_server.WORKSPACE / "data.bin").read_bytes()

    result = mcp_server.read_blob("data.bin")

    assert result["size_bytes"] == 64
    assert result["sha256"] == hashlib.sha256(data).hexdigest()


def test_read_blob_raises_for_missing_file():
    with pytest.raises(FileNotFoundError):
        mcp_server.read_blob("does-not-exist.bin")


def test_read_blob_rejects_path_traversal():
    with pytest.raises(ValueError):
        mcp_server.read_blob("../../etc/passwd")


def test_safe_path_allows_name_within_workspace():
    path = mcp_server._safe_path("nested/name.bin")
    assert path == (mcp_server.WORKSPACE / "nested" / "name.bin").resolve()


def test_safe_path_allows_workspace_root_itself():
    # A name that resolves to the workspace directory itself (e.g. ".") is
    # allowed by the current guard, since it neither escapes nor needs to.
    assert mcp_server._safe_path(".") == mcp_server.WORKSPACE


def test_safe_path_rejects_traversal():
    with pytest.raises(ValueError):
        mcp_server._safe_path("../outside.bin")
