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

"""Async filesystem operations for legacy and sandboxd runtimes."""

import logging
import urllib.parse
from typing import Any, Awaitable, Protocol

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.files.filesystem import (
    Filesystem,
    _STREAM_CHUNK_SIZE,
    _sandboxd_files_endpoint,
)
from k8s_agent_sandbox.models import FileEntry
from k8s_agent_sandbox.trace_manager import async_trace_span, trace


class AsyncBinaryWriter(Protocol):
    """An asynchronous destination that accepts binary file content."""

    def write(self, content: bytes) -> Awaitable[int]:
        """Write content and return the number of accepted bytes."""
        ...


async def _write_all(destination: AsyncBinaryWriter, content: bytes) -> int:
    """Write all content, including to destinations that perform partial writes."""
    written = 0
    while written < len(content):
        count = await destination.write(content[written:])
        if count is None or count <= 0:
            raise OSError("Download destination did not accept file content.")
        if count > len(content) - written:
            raise OSError("Download destination reported an invalid write count.")
        written += count
    return written


class AsyncFilesystem:
    """Read and modify sandbox files without blocking the event loop."""

    def __init__(
        self, connector: AsyncSandboxConnector, tracer: Any, trace_service_name: str
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @async_trace_span("write")
    async def write(
        self,
        path: str, content: bytes | str,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
    ):
        """Write bytes or UTF-8 text to a sandbox-relative path."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            span.set_attribute("sandbox.file.size", len(content))

        if isinstance(content, str):
            content = content.encode("utf-8")

        # Use the same hardened sanitizer as the sync twin — rejects
        # empty / bare-'.', embedded NUL and ASCII control characters,
        # and any '..' segment after normalisation. os.path.basename
        # alone is not sufficient: basename("foo\x00../etc/passwd")
        # returns the string unchanged, and the NUL truncates at the
        # runtime's C layer.
        if not allow_unsafe_paths:
            path = Filesystem._safe_upload_path(path)
        if self.connector.is_sandboxd():
            await self.connector.send_request(
                "PUT",
                _sandboxd_files_endpoint(path),
                content=content,
                headers={"Content-Type": "application/octet-stream"},
                timeout=timeout,
            )
        else:
            files_payload = {"file": (path, content)}
            await self.connector.send_request(
                "POST", "upload", files=files_payload, timeout=timeout
            )
        logging.info(f"File '{path}' uploaded successfully.")

    @async_trace_span("read")
    async def read(
        self,
        path: str,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
    ) -> bytes:
        """Read a sandbox-relative file and return its raw bytes."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        if not allow_unsafe_paths:
            path = Filesystem._safe_upload_path(path)
        endpoint = (
            _sandboxd_files_endpoint(path)
            if self.connector.is_sandboxd()
            else f"download/{urllib.parse.quote(path, safe='')}"
        )
        response = await self.connector.send_request("GET", endpoint, timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content

    @async_trace_span("read_to")
    async def read_to(
        self,
        path: str,
        destination: AsyncBinaryWriter,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
        max_bytes: int | None = None,
    ) -> int:
        """Stream a sandbox file into a caller-owned asynchronous destination.

        The destination is never closed. If ``max_bytes`` is set, at most that
        many bytes are written before an oversized download raises
        ``RuntimeError``. Data written before an error or cancellation remains
        in the destination. The returned value is the number of bytes written.
        """
        if destination is None or not callable(getattr(destination, "write", None)):
            raise TypeError("Download destination must provide a write(bytes) method.")
        if max_bytes is not None:
            if type(max_bytes) is not int:
                raise ValueError("max_bytes must be an integer or None.")
            if max_bytes < 0:
                raise ValueError("max_bytes must be greater than or equal to zero.")

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        if not allow_unsafe_paths:
            path = Filesystem._safe_upload_path(path)
        endpoint = (
            _sandboxd_files_endpoint(path)
            if self.connector.is_sandboxd()
            else f"download/{urllib.parse.quote(path, safe='')}"
        )
        response = await self.connector.send_request(
            "GET", endpoint, timeout=timeout, stream=True
        )
        total = 0
        try:
            content_length = response.headers.get("Content-Length")
            if max_bytes is not None and content_length is not None:
                try:
                    declared_size = int(content_length)
                except (TypeError, ValueError):
                    declared_size = None
                if declared_size is not None and declared_size > max_bytes:
                    raise RuntimeError(
                        f"File size exceeds limit of {max_bytes} bytes."
                    )

            async for chunk in response.aiter_bytes(chunk_size=_STREAM_CHUNK_SIZE):
                if not chunk:
                    continue
                if max_bytes is not None:
                    remaining = max_bytes - total
                    if len(chunk) > remaining:
                        if remaining > 0:
                            total += await _write_all(destination, chunk[:remaining])
                        raise RuntimeError(
                            f"File size exceeds limit of {max_bytes} bytes."
                        )
                total += await _write_all(destination, chunk)
        finally:
            await response.aclose()

        if span.is_recording():
            span.set_attribute("sandbox.file.size", total)
        return total

    @async_trace_span("list")
    async def list(self, path: str, timeout: int = 60) -> list[FileEntry]:
        """List files and directories at a sandbox-relative path."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        # sandboxd wraps directory entries in a DirectoryListing envelope;
        # the legacy runtime returns the entry list directly.
        if self.connector.is_sandboxd():
            response = await self.connector.send_request(
                "GET", _sandboxd_files_endpoint(path), timeout=timeout
            )
        else:
            encoded_path = urllib.parse.quote(path, safe="")
            response = await self.connector.send_request(
                "GET", f"list/{encoded_path}", timeout=timeout
            )

        try:
            entries = response.json()
        except ValueError as e:
            raise RuntimeError(
                f"Failed to decode JSON response from sandbox: {response.text}"
            ) from e

        if self.connector.is_sandboxd():
            if not isinstance(entries, dict) or "entries" not in entries:
                raise RuntimeError(f"Server returned invalid directory listing: {entries}")
            raw_entries = entries["entries"]
            if raw_entries is None:
                raw_entries = []
            elif not isinstance(raw_entries, list):
                raise RuntimeError(f"Server returned invalid directory listing: {entries}")
            file_entries = []
            for entry in raw_entries:
                if not isinstance(entry, dict):
                    raise RuntimeError(
                        f"Server returned invalid directory listing: {entries}"
                    )
                if entry.get("type") not in ("file", "directory"):
                    continue
                try:
                    file_entries.append(FileEntry.from_sandboxd(entry))
                except Exception as e:
                    raise RuntimeError(
                        f"Server returned invalid file entry format: {entry}"
                    ) from e
        else:
            if not entries:
                return []
            try:
                file_entries = [FileEntry.from_legacy(e) for e in entries]
            except Exception as e:
                raise RuntimeError(
                    f"Server returned invalid file entry format: {entries}"
                ) from e

        if span.is_recording():
            span.set_attribute("sandbox.file.count", len(file_entries))
        return file_entries

    @async_trace_span("exists")
    async def exists(self, path: str, timeout: int = 60) -> bool:
        """Return whether a path exists without downloading its contents."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        if self.connector.is_sandboxd():
            response = await self.connector.send_request(
                "HEAD",
                _sandboxd_files_endpoint(path),
                timeout=timeout,
                allowed_statuses={404},
            )
            if response.status_code == 404:
                exists = False
            elif 200 <= response.status_code < 300:
                exists = True
            else:
                raise SandboxRequestError(
                    f"Unexpected status checking sandbox path existence: "
                    f"{response.status_code}",
                    status_code=response.status_code,
                    response=response,
                )
            if span.is_recording():
                span.set_attribute("sandbox.file.exists", exists)
            return exists

        encoded_path = urllib.parse.quote(path, safe="")
        response = await self.connector.send_request(
            "GET", f"exists/{encoded_path}", timeout=timeout
        )

        try:
            response_data = response.json()
        except ValueError as e:
            raise RuntimeError(
                f"Failed to decode JSON response from sandbox: {response.text}"
            ) from e

        exists = response_data.get("exists", False)
        if span.is_recording():
            span.set_attribute("sandbox.file.exists", exists)
        return exists

    @async_trace_span("delete")
    async def delete(
        self, path: str, recursive: bool = False, timeout: int = 60
    ) -> None:
        """Delete a sandboxd path, optionally including directory contents.

        The legacy runtime has no delete endpoint and raises
        ``NotImplementedError``.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        if not self.connector.is_sandboxd():
            raise NotImplementedError(
                "delete() is only supported by the sandboxd runtime; the legacy "
                "python-runtime has no delete endpoint"
            )
        if path == "":
            raise ValueError("delete: path must not be empty")
        endpoint = _sandboxd_files_endpoint(path)
        if recursive:
            endpoint += "?recursive=true"
        await self.connector.send_request("DELETE", endpoint, timeout=timeout)
        logging.info(f"Path '{path}' deleted successfully.")
