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

"""Tests for filesystem path safety and runtime-specific operations."""

import asyncio
import io
import unittest
import urllib.parse
from unittest.mock import AsyncMock, MagicMock, patch

from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.files.async_filesystem import AsyncFilesystem
from k8s_agent_sandbox.files.filesystem import Filesystem, _sandboxd_files_endpoint


class PartialWriter:
    def __init__(self, max_write: int):
        self.max_write = max_write
        self.content = bytearray()
        self.closed = False

    def write(self, content: bytes) -> int:
        accepted = min(len(content), self.max_write)
        self.content.extend(content[:accepted])
        return accepted

    def close(self):
        self.closed = True


class FailingWriter:
    def write(self, content: bytes) -> int:
        raise OSError("destination failed")


class AsyncPartialWriter:
    def __init__(self, max_write: int):
        self.max_write = max_write
        self.content = bytearray()
        self.closed = False

    async def write(self, content: bytes) -> int:
        accepted = min(len(content), self.max_write)
        self.content.extend(content[:accepted])
        return accepted

    async def close(self):
        self.closed = True


class AsyncFailingWriter:
    async def write(self, content: bytes) -> int:
        raise OSError("destination failed")


def streaming_response(chunks: list[bytes], content_length: int | None = None):
    response = MagicMock()
    response.status_code = 200
    response.headers = {}
    if content_length is not None:
        response.headers["Content-Length"] = str(content_length)
    response.iter_content.return_value = iter(chunks)
    return response


def async_streaming_response(
    chunks: list[bytes], content_length: int | None = None
):
    response = MagicMock()
    response.headers = {}
    if content_length is not None:
        response.headers["Content-Length"] = str(content_length)

    async def iterate(*, chunk_size: int):
        del chunk_size
        for chunk in chunks:
            yield chunk

    response.aiter_bytes = iterate
    response.aclose = AsyncMock()
    return response


class TestFilesystemSafeUploadPath(unittest.TestCase):
    """SDK must sanitize multipart filenames so the runtime cannot be
    tricked into writing outside its base directory."""

    def test_basename_is_preserved(self):
        self.assertEqual(Filesystem._safe_upload_path("foo.txt"), "foo.txt")

    def test_relative_subpath_is_preserved(self):
        self.assertEqual(Filesystem._safe_upload_path("dir/foo.txt"), "dir/foo.txt")

    def test_filename_spaces_are_preserved(self) -> None:
        for path in (" leading.txt", "trailing.txt ", "dir/ both.txt "):
            with self.subTest(path=path):
                self.assertEqual(Filesystem._safe_upload_path(path), path)

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

    def test_empty_path_is_rejected(self) -> None:
        for path in ("", " ", "   "):
            with self.subTest(path=path):
                with self.assertRaisesRegex(ValueError, "empty"):
                    Filesystem._safe_upload_path(path)

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

    def test_sandboxd_dot_segments_are_encoded(self):
        self.assertEqual(_sandboxd_files_endpoint("."), "v1/files/%2E")
        self.assertEqual(_sandboxd_files_endpoint(".."), "v1/files/%2E%2E")


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


class TestFilesystemSafePaths(unittest.TestCase):
    def setUp(self):
        self._connector = MagicMock()
        # These cases assert the legacy python-runtime wire shape
        # (multipart upload / download path); pin the runtime to legacy.
        self._connector.is_sandboxd.return_value = False
        tracer = MagicMock()
        self._fs = Filesystem(self._connector, tracer, trace_service_name="test")

    def _make_async_fs(self) -> AsyncFilesystem:
        self._connector = MagicMock()
        self._connector.send_request = AsyncMock()
        tracer = AsyncMock()
        return AsyncFilesystem(self._connector, tracer, trace_service_name="test")

    def _get_path_from_last_connector_upload_request(self):
        return self._connector.send_request.call_args.kwargs["files"]["file"][0]

    def _get_path_from_last_connector_download_request(self):
        quoted_request_path = self._connector.send_request.call_args.args[1]
        _, quoted_file_path = quoted_request_path.split("/")
        return urllib.parse.unquote(quoted_file_path)

    def _do_write(self, **kwargs):
        self._fs.write("/dir/foo.txt", "some content", **kwargs)

    def _do_read(self, **kwargs):
        self._fs.read("/dir/foo.txt", **kwargs)

    def test_write_file_paths(self):
        self._do_write()
        assert self._get_path_from_last_connector_upload_request() == "dir/foo.txt"

    def test_write_file_unsafe_paths(self):
        self._do_write(allow_unsafe_paths=True)
        assert self._get_path_from_last_connector_upload_request() == "/dir/foo.txt"

    def test_read_file_paths(self):
        self._do_read()
        assert self._get_path_from_last_connector_download_request() == "dir/foo.txt"

    def test_read_file_unsafe_paths(self):
        self._do_read(allow_unsafe_paths=True)
        assert self._get_path_from_last_connector_download_request() == "/dir/foo.txt"


class TestAsyncFilesystemSafePaths(TestFilesystemSafePaths):
    def setUp(self):
        self._connector = MagicMock()
        self._connector.send_request = AsyncMock()
        self._connector.is_sandboxd.return_value = False
        tracer = MagicMock()
        self._fs = AsyncFilesystem(self._connector, tracer, trace_service_name="test")

    def _do_write(self, **kwargs):
        asyncio.run(self._fs.write("/dir/foo.txt", "some content", **kwargs))

    def _do_read(self, **kwargs):
        asyncio.run(self._fs.read("/dir/foo.txt", **kwargs))


class TestAsyncSandboxdFilesystem(unittest.IsolatedAsyncioTestCase):
    """Verify sandboxd endpoints, response validation, and tracing."""

    def setUp(self):
        self.connector = MagicMock()
        self.connector.send_request = AsyncMock()
        self.connector.is_sandboxd.return_value = True
        self.fs = AsyncFilesystem(self.connector, MagicMock(), trace_service_name="test")

    async def test_write_uses_sandboxd_put(self):
        await self.fs.write("dir/script.py", b"print(1)")
        args, kwargs = self.connector.send_request.call_args
        self.assertEqual(args[:2], ("PUT", "v1/files/dir%2Fscript.py"))
        self.assertEqual(kwargs["content"], b"print(1)")
        self.assertNotIn("data", kwargs)

    async def test_read_uses_sandboxd_get(self):
        response = MagicMock(content=b"hello")
        self.connector.send_request.return_value = response

        self.assertEqual(await self.fs.read("notes/hello.txt"), b"hello")
        args, _ = self.connector.send_request.call_args
        self.assertEqual(args[:2], ("GET", "v1/files/notes%2Fhello.txt"))

    async def test_list_unwraps_sandboxd_directory_listing(self):
        response = MagicMock()
        response.json.return_value = {
            "path": "/notes",
            "entries": [
                {
                    "name": "a.txt",
                    "size": 5,
                    "type": "file",
                    "modified_at": "2026-08-06T10:00:00Z",
                    "mode": "0644",
                }
            ],
        }
        self.connector.send_request.return_value = response

        entries = await self.fs.list("notes")

        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0].name, "a.txt")
        self.assertEqual(entries[0].mode, "0644")

    async def test_list_rejects_non_list_entries(self):
        response = MagicMock()
        response.json.return_value = {"path": "/notes", "entries": {}}
        self.connector.send_request.return_value = response

        with self.assertRaisesRegex(RuntimeError, "invalid directory listing"):
            await self.fs.list("notes")

    async def test_list_rejects_non_object_entry(self):
        response = MagicMock()
        response.json.return_value = {"path": "/notes", "entries": ["a.txt"]}
        self.connector.send_request.return_value = response

        with self.assertRaisesRegex(RuntimeError, "invalid directory listing"):
            await self.fs.list("notes")

    async def test_list_treats_null_entries_as_empty(self):
        response = MagicMock()
        response.json.return_value = {"path": "/notes", "entries": None}
        self.connector.send_request.return_value = response

        self.assertEqual(await self.fs.list("notes"), [])

    async def test_exists_allows_sandboxd_404(self):
        response = MagicMock(status_code=404)
        self.connector.send_request.return_value = response

        self.assertFalse(await self.fs.exists("missing.txt"))
        _, kwargs = self.connector.send_request.call_args
        self.assertEqual(kwargs["allowed_statuses"], {404})

    async def test_exists_rejects_non_success_status(self):
        response = MagicMock(status_code=304)
        self.connector.send_request.return_value = response

        with self.assertRaises(SandboxRequestError):
            await self.fs.exists("maybe.txt")

    async def test_delete_uses_sandboxd_endpoint(self):
        await self.fs.delete("dir", recursive=True)
        args, _ = self.connector.send_request.call_args
        self.assertEqual(args[:2], ("DELETE", "v1/files/dir?recursive=true"))

    async def test_delete_rejects_empty_path(self):
        with self.assertRaisesRegex(ValueError, "path must not be empty"):
            await self.fs.delete("")

        self.connector.send_request.assert_not_awaited()

    @patch("k8s_agent_sandbox.files.async_filesystem.trace")
    async def test_delete_records_path_in_trace(self, mock_trace):
        span = MagicMock()
        span.is_recording.return_value = True
        mock_trace.get_current_span.return_value = span

        await self.fs.delete("dir")

        span.set_attribute.assert_called_once_with("sandbox.file.path", "dir")

class TestFilesystemStreamingRead(unittest.TestCase):
    def setUp(self):
        self.connector = MagicMock()
        self.connector.is_sandboxd.return_value = False
        self.filesystem = Filesystem(
            self.connector, MagicMock(), trace_service_name="test"
        )

    def test_read_to_streams_chunks_and_handles_partial_writes(self):
        chunks = [b"a" * (64 * 1024), b"b" * (64 * 1024)]
        response = streaming_response(chunks)
        self.connector.send_request.return_value = response
        destination = PartialWriter(max_write=8192)

        written = self.filesystem.read_to("dir/file.bin", destination)

        self.assertEqual(written, 128 * 1024)
        self.assertEqual(destination.content, b"".join(chunks))
        self.assertFalse(destination.closed)
        response.close.assert_called_once_with()
        self.connector.send_request.assert_called_once_with(
            "GET", "download/dir%2Ffile.bin", timeout=60, stream=True
        )

    def test_read_to_supports_sandboxd(self):
        self.connector.is_sandboxd.return_value = True
        response = streaming_response([b"sandboxd"])
        self.connector.send_request.return_value = response
        destination = io.BytesIO()

        written = self.filesystem.read_to("dir/file.bin", destination)

        self.assertEqual(written, 8)
        self.assertEqual(destination.getvalue(), b"sandboxd")
        self.connector.send_request.assert_called_once_with(
            "GET", "v1/files/dir%2Ffile.bin", timeout=60, stream=True
        )

    def test_read_to_rejects_non_success_status(self):
        response = streaming_response([b"error"])
        response.status_code = 300
        self.connector.send_request.return_value = response
        destination = io.BytesIO()

        with self.assertRaises(SandboxRequestError) as ctx:
            self.filesystem.read_to("file.bin", destination)

        self.assertEqual(ctx.exception.status_code, 300)
        self.assertEqual(destination.getvalue(), b"")
        response.close.assert_called_once_with()

    def test_read_to_enforces_unknown_length_limit_while_streaming(self):
        response = streaming_response([b"abc", b"def"])
        self.connector.send_request.return_value = response
        destination = io.BytesIO()

        with self.assertRaisesRegex(RuntimeError, "exceeds limit"):
            self.filesystem.read_to("file.bin", destination, max_bytes=4)

        self.assertEqual(destination.getvalue(), b"abcd")
        response.close.assert_called_once_with()

    def test_read_to_rejects_declared_oversize_before_writing(self):
        response = streaming_response([b"ignored"], content_length=7)
        self.connector.send_request.return_value = response
        destination = io.BytesIO()

        with self.assertRaisesRegex(RuntimeError, "exceeds limit"):
            self.filesystem.read_to("file.bin", destination, max_bytes=6)

        self.assertEqual(destination.getvalue(), b"")
        response.close.assert_called_once_with()

    def test_read_to_closes_response_after_destination_error(self):
        response = streaming_response([b"content"])
        self.connector.send_request.return_value = response

        with self.assertRaisesRegex(OSError, "destination failed"):
            self.filesystem.read_to("file.bin", FailingWriter())

        response.close.assert_called_once_with()

    def test_read_to_rejects_invalid_destination_and_limit(self):
        with self.assertRaisesRegex(TypeError, "write"):
            self.filesystem.read_to("file.bin", None)
        with self.assertRaisesRegex(ValueError, "max_bytes"):
            self.filesystem.read_to("file.bin", io.BytesIO(), max_bytes=-1)
        for invalid_limit in (1.5, True, "10"):
            with self.subTest(max_bytes=invalid_limit):
                with self.assertRaisesRegex(ValueError, "integer"):
                    self.filesystem.read_to(
                        "file.bin", io.BytesIO(), max_bytes=invalid_limit
                    )
        self.connector.send_request.assert_not_called()


class TestAsyncFilesystemStreamingRead(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.connector = MagicMock()
        self.connector.send_request = AsyncMock()
        self.connector.is_sandboxd.return_value = False
        self.filesystem = AsyncFilesystem(
            self.connector, MagicMock(), trace_service_name="test"
        )

    async def test_read_to_streams_chunks_and_handles_partial_writes(self):
        chunks = [b"a" * (64 * 1024), b"b" * (64 * 1024)]
        response = async_streaming_response(chunks)
        self.connector.send_request.return_value = response
        destination = AsyncPartialWriter(max_write=8192)

        written = await self.filesystem.read_to("dir/file.bin", destination)

        self.assertEqual(written, 128 * 1024)
        self.assertEqual(destination.content, b"".join(chunks))
        self.assertFalse(destination.closed)
        response.aclose.assert_awaited_once_with()
        self.connector.send_request.assert_awaited_once_with(
            "GET", "download/dir%2Ffile.bin", timeout=60, stream=True
        )

    async def test_read_to_supports_sandboxd(self):
        self.connector.is_sandboxd.return_value = True
        response = async_streaming_response([b"sandboxd"])
        self.connector.send_request.return_value = response
        destination = AsyncPartialWriter(max_write=10)

        written = await self.filesystem.read_to("dir/file.bin", destination)

        self.assertEqual(written, 8)
        self.assertEqual(destination.content, b"sandboxd")
        self.connector.send_request.assert_awaited_once_with(
            "GET", "v1/files/dir%2Ffile.bin", timeout=60, stream=True
        )

    async def test_read_to_enforces_unknown_length_limit_while_streaming(self):
        response = async_streaming_response([b"abc", b"def"])
        self.connector.send_request.return_value = response
        destination = AsyncPartialWriter(max_write=10)

        with self.assertRaisesRegex(RuntimeError, "exceeds limit"):
            await self.filesystem.read_to("file.bin", destination, max_bytes=4)

        self.assertEqual(destination.content, b"abcd")
        response.aclose.assert_awaited_once_with()

    async def test_read_to_closes_response_when_cancelled(self):
        response = async_streaming_response([b"content"])
        self.connector.send_request.return_value = response
        destination = MagicMock()
        destination.write = AsyncMock(side_effect=asyncio.CancelledError)

        with self.assertRaises(asyncio.CancelledError):
            await self.filesystem.read_to("file.bin", destination)

        response.aclose.assert_awaited_once_with()

    async def test_read_to_closes_response_after_destination_error(self):
        response = async_streaming_response([b"content"])
        self.connector.send_request.return_value = response

        with self.assertRaisesRegex(OSError, "destination failed"):
            await self.filesystem.read_to("file.bin", AsyncFailingWriter())

        response.aclose.assert_awaited_once_with()

    async def test_read_to_rejects_declared_oversize_before_writing(self):
        response = async_streaming_response([b"ignored"], content_length=7)
        self.connector.send_request.return_value = response
        destination = AsyncPartialWriter(max_write=10)

        with self.assertRaisesRegex(RuntimeError, "exceeds limit"):
            await self.filesystem.read_to("file.bin", destination, max_bytes=6)

        self.assertEqual(destination.content, b"")
        response.aclose.assert_awaited_once_with()

    async def test_read_to_rejects_invalid_destination_and_limit(self):
        with self.assertRaisesRegex(TypeError, "write"):
            await self.filesystem.read_to("file.bin", None)
        with self.assertRaisesRegex(ValueError, "max_bytes"):
            await self.filesystem.read_to(
                "file.bin", AsyncPartialWriter(max_write=10), max_bytes=-1
            )
        for invalid_limit in (1.5, True, "10"):
            with self.subTest(max_bytes=invalid_limit):
                with self.assertRaisesRegex(ValueError, "integer"):
                    await self.filesystem.read_to(
                        "file.bin",
                        AsyncPartialWriter(max_write=10),
                        max_bytes=invalid_limit,
                    )
        self.connector.send_request.assert_not_awaited()

if __name__ == '__main__':
    unittest.main()
