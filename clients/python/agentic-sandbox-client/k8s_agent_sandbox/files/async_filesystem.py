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

"""Asynchronous filesystem operations for sandbox runtimes."""

import asyncio
import logging
import urllib.parse
from collections.abc import AsyncIterator
from typing import Any, BinaryIO

from k8s_agent_sandbox.async_connector import AsyncSandboxConnector
from k8s_agent_sandbox.files.filesystem import (
    Filesystem,
    _multipart_framing,
    _read_binary_chunk,
    _validate_binary_stream,
)
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.files.filesystem import _sandboxd_files_endpoint
from k8s_agent_sandbox.models import FileEntry
from k8s_agent_sandbox.trace_manager import async_trace_span, trace


async def _iter_multipart_body(
    prefix: bytes,
    content: BinaryIO,
    suffix: bytes,
) -> AsyncIterator[bytes]:
    yield prefix
    while chunk := await asyncio.to_thread(_read_binary_chunk, content):
        yield chunk
    yield suffix


async def _iter_binary_chunks(content: BinaryIO) -> AsyncIterator[bytes]:
    """Read a binary stream in bounded chunks without blocking the loop."""
    while chunk := await asyncio.to_thread(_read_binary_chunk, content):
        yield chunk


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
        path: str,
        content: bytes | str | BinaryIO,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
    ) -> None:
        if isinstance(content, str):
            content = content.encode("utf-8")
        elif isinstance(content, (bytearray, memoryview)):
            content = bytes(content)
        elif not isinstance(content, bytes):
            _validate_binary_stream(content)
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
            if isinstance(content, bytes):
                span.set_attribute("sandbox.file.size", len(content))

        # Use the same hardened sanitizer as the sync twin — rejects
        # empty / bare-'.', embedded NUL and ASCII control characters,
        # and any '..' segment after normalisation. os.path.basename
        # alone is not sufficient: basename("foo\x00../etc/passwd")
        # returns the string unchanged, and the NUL truncates at the
        # runtime's C layer.
        if not allow_unsafe_paths:
            path = Filesystem._safe_upload_path(path)
        if self.connector.is_sandboxd():
            if isinstance(content, bytes):
                await self.connector.send_request(
                    "PUT",
                    _sandboxd_files_endpoint(path),
                    content=content,
                    headers={"Content-Type": "application/octet-stream"},
                    timeout=timeout,
                )
            else:
                await self.connector.send_request(
                    "PUT",
                    _sandboxd_files_endpoint(path),
                    content=_iter_binary_chunks(content),
                    headers={"Content-Type": "application/octet-stream"},
                    timeout=timeout,
                )
        elif isinstance(content, bytes):
            files_payload = {"file": (path, content)}
            await self.connector.send_request(
                "POST", "upload", files=files_payload, timeout=timeout
            )
        else:
            prefix, suffix, content_type = _multipart_framing(path)
            # This async iterator is one-shot. AsyncSandboxConnector must not
            # add automatic POST retries, which would replay it as an empty body.
            await self.connector.send_request(
                "POST",
                "upload",
                content=_iter_multipart_body(prefix, content, suffix),
                headers={"Content-Type": content_type},
                timeout=timeout,
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
