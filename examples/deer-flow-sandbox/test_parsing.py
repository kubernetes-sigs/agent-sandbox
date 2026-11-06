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

"""Unit tests for the cluster-free parsing helpers."""

import pytest

from agent_sandbox_provider import parsing


def test_to_root_relative_strips_prefix():
    assert parsing.to_root_relative("/mnt/user-data/workspace/fib.py", "/mnt/user-data") == "workspace/fib.py"


def test_to_root_relative_root_itself():
    assert parsing.to_root_relative("/mnt/user-data", "/mnt/user-data") == "."


def test_to_root_relative_rejects_outside_path():
    with pytest.raises(PermissionError):
        parsing.to_root_relative("/etc/passwd", "/mnt/user-data")


def test_to_root_relative_rejects_traversal_out_of_root():
    # A prefix-only check would let this through: the path starts with the
    # root but normalizes to /etc.
    with pytest.raises(PermissionError):
        parsing.to_root_relative("/mnt/user-data/../../etc", "/mnt/user-data")


def test_to_root_relative_normalizes_inside_root():
    assert (
        parsing.to_root_relative("/mnt/user-data/workspace/../workspace/fib.py", "/mnt/user-data")
        == "workspace/fib.py"
    )
    assert parsing.to_root_relative("/mnt/user-data/workspace//sub/", "/mnt/user-data") == "workspace/sub"
    assert parsing.to_root_relative("/mnt/user-data/", "/mnt/user-data") == "."


def test_slice_lines_full_content():
    # Harness read-file contract: windows are re-joined with "\n" (line
    # terminators are dropped), matching split_file_lines usage in the
    # shipped remote providers.
    assert parsing.slice_lines("a\nb\n", None, None) == "a\nb"
    assert parsing.slice_lines("a\nb", None, None) == "a\nb"


def test_slice_lines_window_is_one_indexed_inclusive():
    assert parsing.slice_lines("a\nb\nc\n", 1, 1) == "a"
    assert parsing.slice_lines("a\nb\nc\n", 2, 3) == "b\nc"


def test_slice_lines_start_beyond_end_returns_empty():
    assert parsing.slice_lines("a\nb\n", 2, 1) == ""


def test_slice_lines_rejects_start_below_one():
    with pytest.raises(ValueError):
        parsing.slice_lines("a\n", 0, 1)


def test_slice_lines_crlf_strips_trailing_cr():
    # The contract (deerflow.sandbox.read_file_contract.split_file_lines)
    # forbids splitlines(): lines end only at "\n" and a trailing "\r" is
    # stripped, so remote windows agree with count_file_lines.
    assert parsing.slice_lines("a\r\nb\r\n", 1, 2) == "a\nb"
    assert parsing.slice_lines("a\r\nb\r\n", 2, 2) == "b"


def test_slice_lines_lone_cr_is_content_not_a_separator():
    # splitlines() would report three lines here; "\r" inside a line is
    # ordinary content.
    assert parsing.slice_lines("a\rb\nc\n", 1, 2) == "a\rb\nc"
    assert parsing.slice_lines("a\rb\nc\n", 2, 2) == "c"


def test_slice_lines_other_unicode_line_breaks_are_content():
    # form feed, vertical tab, NEL and U+2028 must not end lines either
    # (splitlines treats them as breaks; the contract does not).
    content = "a\x0cb\x0b\x85\u2028c\nsecond\n"
    assert parsing.slice_lines(content, 1, 1) == "a\x0cb\x0b\x85\u2028c"
    assert parsing.slice_lines(content, 2, 2) == "second"


def test_slice_lines_last_line_without_newline_is_addressable():
    assert parsing.slice_lines("a\nb", 2, 2) == "b"


def test_parse_grep_line_typical():
    assert parsing.parse_grep_line("workspace/fib.py:3:def fib():") == ("workspace/fib.py", 3, "def fib():")


def test_parse_grep_line_content_with_colons():
    assert parsing.parse_grep_line("f.py:10:d = {'a': 1}") == ("f.py", 10, "d = {'a': 1}")


def test_parse_grep_line_binary_notice_returns_none():
    assert parsing.parse_grep_line("Binary file x.bin matches") is None
