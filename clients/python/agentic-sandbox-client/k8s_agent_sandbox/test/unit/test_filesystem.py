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


import asyncio
import unittest
from unittest.mock import MagicMock

from k8s_agent_sandbox.files.async_filesystem import AsyncFilesystem
from k8s_agent_sandbox.files.filesystem import Filesystem


class TestFilesystemSafeUploadPath(unittest.TestCase):
    """SDK must sanitize multipart filenames so the runtime cannot be
    tricked into writing outside its base directory."""

    def test_basename_is_preserved(self):
        self.assertEqual(Filesystem._safe_upload_path("foo.txt"), "foo.txt")

    def test_relative_subpath_is_preserved(self):
        self.assertEqual(Filesystem._safe_upload_path("dir/foo.txt"), "dir/foo.txt")

    def test_leading_slash_is_stripped(self):
        # An absolute-looking path gets normalized to a relative path under the runtime root.
        self.assertEqual(Filesystem._safe_upload_path("/dir/foo.txt"), "dir/foo.txt")

    def test_double_slash_collapses(self):
        self.assertEqual(Filesystem._safe_upload_path("dir//foo.txt"), "dir/foo.txt")

    def test_parent_traversal_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "escapes the sandbox root"):
            Filesystem._safe_upload_path("../etc/passwd")

    def test_embedded_parent_traversal_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "escapes the sandbox root"):
            Filesystem._safe_upload_path("dir/../../etc/passwd")

    def test_absolute_etc_is_not_allowed_to_escape(self):
        # /etc/passwd normalizes to "etc/passwd" relative to the runtime root.
        self.assertEqual(Filesystem._safe_upload_path("/etc/passwd"), "etc/passwd")

    def test_empty_path_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "empty"):
            Filesystem._safe_upload_path("")

    def test_bare_dot_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "does not name a file"):
            Filesystem._safe_upload_path(".")

    def test_embedded_nul_is_rejected(self):
        # os.path.normpath keeps embedded NULs intact, and the NUL byte
        # truncates at the runtime's C/syscall layer — without the
        # control-char check, "foo\x00../etc/passwd" would survive the
        # "..-in-parts" filter (no segment equals "..") and then silently
        # resolve to "foo" on the server.
        with self.assertRaisesRegex(ValueError, "control characters"):
            Filesystem._safe_upload_path("foo\x00../etc/passwd")

    def test_control_chars_are_rejected(self):
        # Newlines, tabs, form feeds etc. can split HTTP headers or
        # confuse multipart parsers downstream.
        for bad in ("foo\nbar.txt", "foo\tbar.txt", "foo\rbar.txt"):
            with self.assertRaisesRegex(ValueError, "control characters"):
                Filesystem._safe_upload_path(bad)


class TestAsyncFilesystemSafeUploadPath(unittest.TestCase):
    """The async twin must apply the same sanitizer as the sync one —
    otherwise the NUL-truncation / '..' escape vector is only half-fixed.
    """

    def _make_fs(self) -> AsyncFilesystem:
        connector = MagicMock()
        tracer = MagicMock()
        return AsyncFilesystem(connector, tracer, trace_service_name="test")

    def test_async_write_rejects_embedded_nul(self):
        fs = self._make_fs()
        with self.assertRaisesRegex(ValueError, "control characters"):
            asyncio.run(fs.write("foo\x00../etc/passwd", b"payload"))

    def test_async_write_rejects_parent_traversal(self):
        fs = self._make_fs()
        with self.assertRaisesRegex(ValueError, "escapes the sandbox root"):
            asyncio.run(fs.write("../etc/passwd", b"payload"))


if __name__ == '__main__':
    unittest.main()
