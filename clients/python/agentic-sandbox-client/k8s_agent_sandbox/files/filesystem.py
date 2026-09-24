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

"""Synchronous filesystem operations for legacy and sandboxd runtimes."""
import logging
import posixpath
import urllib.parse
from typing import Any, List, Protocol

from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.models import FileEntry
from k8s_agent_sandbox.trace_manager import trace, trace_span



_STREAM_CHUNK_SIZE = 64 * 1024


class BinaryWriter(Protocol):
    """A synchronous destination that accepts binary file content."""

    def write(self, content: bytes) -> int:
        """Write content and return the number of accepted bytes."""
        ...


def _write_all(destination: BinaryWriter, content: bytes) -> int:
    """Write all content, including to destinations that perform partial writes."""
    written = 0
    while written < len(content):
        count = destination.write(content[written:])
        if count is None or count <= 0:
            raise OSError("Download destination did not accept file content.")
        if count > len(content) - written:
            raise OSError("Download destination reported an invalid write count.")
        written += count
    return written


def _sandboxd_files_endpoint(path: str) -> str:
    """Return the sandboxd REST path for a sandbox-relative file path."""
    encoded = []
    for segment in path.split("/"):
        if segment == ".":
            encoded.append("%2E")
        elif segment == "..":
            encoded.append("%2E%2E")
        else:
            encoded.append(urllib.parse.quote(segment, safe=""))
    return f"v1/files/{'%2F'.join(encoded)}"


class Filesystem:
    """
    Handles file operations within the sandbox.

    Speaks either the legacy python-runtime HTTP API or the sandboxd
    Filesystem & Runtime REST API, selected by the connection config
    (``connector.is_sandboxd()``).
    """
    def __init__(
        self, connector: SandboxConnector, tracer: Any, trace_service_name: str
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name

    @trace_span("write")
    def write(
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

        # The sandbox runtime uses the multipart ``filename`` field as a
        # relative destination path under its base directory (e.g. /app).
        # ``os.path.join`` on the server will honor absolute paths and
        # ``..`` segments, so a caller could otherwise escape the
        # confinement by sending filename='/etc/passwd' or '../etc/...'.
        # Sanitize here to guarantee the filename is a normalized
        # relative path with no upward traversal.
        if not allow_unsafe_paths:
            path = self._safe_upload_path(path)

        if self.connector.is_sandboxd():
            # sandboxd write is an idempotent PUT of the raw bytes; parent
            # directories are created server-side (temp-file + rename).
            self.connector.send_request(
                "PUT", _sandboxd_files_endpoint(path),
                data=content,
                headers={"Content-Type": "application/octet-stream"},
                timeout=timeout,
            )
        else:
            files_payload = {'file': (path, content)}
            self.connector.send_request("POST", "upload",
                          files=files_payload, timeout=timeout)
        logging.info(f"File '{path}' uploaded successfully.")

    @staticmethod
    def _safe_upload_path(path: str) -> str:
        """Return a relative, ``..``-free filename safe to send as multipart filename.

        Rejects NUL bytes and ASCII control characters before normalisation:
        ``os.path.normpath`` preserves embedded NULs, and a NUL in the
        filename truncates at the runtime's C/syscall layer. Without this
        check ``foo\\x00../etc/passwd`` would survive the ``..`` split (no
        part equals ``".."`` because the NUL-prefixed segment doesn't
        match) yet resolve to ``foo`` on the filesystem — or worse,
        something server-dependent.``.
        """
        if any(ord(c) < 0x20 or ord(c) == 0x7F for c in path):
            raise ValueError(
                f"Upload path contains ASCII control characters: {path!r}"
            )
        if not path.strip():
            raise ValueError("Upload path cannot be empty.")

        normalized = posixpath.normpath(path).lstrip("/")
        if not normalized or normalized == ".":
            raise ValueError(f"Upload path '{path}' does not name a file.")
        parts = normalized.split("/")
        if any(part == ".." for part in parts):
            raise ValueError(
                f"Upload path '{path}' escapes the sandbox root."
            )
        return normalized

    @trace_span("read")
    def read(
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
            path = self._safe_upload_path(path)

        if self.connector.is_sandboxd():
            endpoint = _sandboxd_files_endpoint(path)
        else:
            endpoint = f"download/{urllib.parse.quote(path, safe='')}"
        response = self.connector.send_request("GET", endpoint, timeout=timeout)
        content = response.content

        if span.is_recording():
            span.set_attribute("sandbox.file.size", len(content))

        return content

    @trace_span("read_to")
    def read_to(
        self,
        path: str,
        destination: BinaryWriter,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
        max_bytes: int | None = None,
    ) -> int:
        """Stream a sandbox file into a caller-owned binary destination.

        The destination is never closed. If ``max_bytes`` is set, at most that
        many bytes are written before an oversized download raises
        ``RuntimeError``. Data written before an error remains in the
        destination. The returned value is the number of bytes written.
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
            path = self._safe_upload_path(path)

        if self.connector.is_sandboxd():
            endpoint = _sandboxd_files_endpoint(path)
        else:
            endpoint = f"download/{urllib.parse.quote(path, safe='')}"

        response = self.connector.send_request(
            "GET", endpoint, timeout=timeout, stream=True
        )
        total = 0
        try:
            if not 200 <= response.status_code < 300:
                raise SandboxRequestError(
                    f"Unexpected status downloading sandbox file: {response.status_code}",
                    status_code=response.status_code,
                    response=response,
                )
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

            for chunk in response.iter_content(chunk_size=_STREAM_CHUNK_SIZE):
                if not chunk:
                    continue
                if max_bytes is not None:
                    remaining = max_bytes - total
                    if len(chunk) > remaining:
                        if remaining > 0:
                            total += _write_all(destination, chunk[:remaining])
                        raise RuntimeError(
                            f"File size exceeds limit of {max_bytes} bytes."
                        )
                total += _write_all(destination, chunk)
        finally:
            response.close()

        if span.is_recording():
            span.set_attribute("sandbox.file.size", total)
        return total

    @trace_span("list")
    def list(self, path: str, timeout: int = 60) -> List[FileEntry]:
        """List files and directories at a sandbox-relative path."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')

        if self.connector.is_sandboxd():
            response = self.connector.send_request(
                "GET", _sandboxd_files_endpoint(path), timeout=timeout)
            try:
                listing = response.json()
            except ValueError as e:
                raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e
            # A directory listing is a DirectoryListing envelope; reject
            # anything else rather than silently returning an empty list.
            if not isinstance(listing, dict) or "entries" not in listing:
                raise RuntimeError(f"Server returned invalid directory listing: {listing}")
            file_entries = []
            for entry in listing.get("entries") or []:
                # Skip entry types the SDK model does not represent (e.g. a
                # stray "symlink") so one unknown entry does not fail the
                # whole listing.
                if entry.get("type") not in ("file", "directory"):
                    logging.info(f"Skipping unsupported file entry type: {entry.get('type')!r}")
                    continue
                try:
                    file_entries.append(FileEntry.from_sandboxd(entry))
                except Exception as ex:
                    raise RuntimeError(f"Server returned invalid file entry format: {entry}") from ex
        else:
            response = self.connector.send_request("GET", f"list/{encoded_path}", timeout=timeout)
            try:
                entries = response.json()
            except ValueError as e:
                raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e
            if not entries:
                return []
            try:
                file_entries = [FileEntry.from_legacy(e) for e in entries]
            except Exception as e:
                raise RuntimeError(f"Server returned invalid file entry format: {entries}") from e

        if span.is_recording():
            span.set_attribute("sandbox.file.count", len(file_entries))
        return file_entries

    @trace_span("exists")
    def exists(self, path: str, timeout: int = 60) -> bool:
        """Return whether a path exists without downloading its contents."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        encoded_path = urllib.parse.quote(path, safe='')

        if self.connector.is_sandboxd():
            # sandboxd has no exists endpoint: HEAD answers existence
            # (200 vs 404) without transferring the body. 404 is passed via
            # allowed_statuses so it is returned instead of becoming a
            # SandboxRequestError.
            response = self.connector.send_request(
                "HEAD", _sandboxd_files_endpoint(path),
                timeout=timeout, allowed_statuses={404})
            exists = response.status_code != 404
            if span.is_recording():
                span.set_attribute("sandbox.file.exists", exists)
            return exists

        response = self.connector.send_request("GET", f"exists/{encoded_path}", timeout=timeout)
        try:
            response_data = response.json()
        except ValueError as e:
            raise RuntimeError(f"Failed to decode JSON response from sandbox: {response.text}") from e

        exists = response_data.get("exists", False)
        if span.is_recording():
            span.set_attribute("sandbox.file.exists", exists)
        return exists

    @trace_span("delete")
    def delete(self, path: str, recursive: bool = False, timeout: int = 60) -> None:
        """Remove a file or directory. sandboxd runtime only.

        With ``recursive=True`` directories are removed with their contents;
        otherwise deleting a non-empty directory fails with a 409. The legacy
        python-runtime has no delete endpoint and raises NotImplementedError.
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
        self.connector.send_request("DELETE", endpoint, timeout=timeout)
        logging.info(f"Path '{path}' deleted successfully.")
