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

import io
import logging
import posixpath
import secrets
import urllib.parse
from collections.abc import Iterator, Mapping
from typing import Any, BinaryIO, List, Protocol

from urllib3.fields import RequestField

import requests

from k8s_agent_sandbox.connector import SandboxConnector
from k8s_agent_sandbox.exceptions import SandboxRequestError, SandboxResponseTooLargeError
from k8s_agent_sandbox.models import FileEntry, SandboxFileConfig
from k8s_agent_sandbox.trace_manager import trace, trace_span

logger = logging.getLogger(__name__)

_STREAM_CHUNK_SIZE = 64 * 1024


def _download_limit(config: SandboxFileConfig, max_bytes: int | None) -> int | None:
    """A per-call limit may tighten, but never loosen, the instance limit."""
    if max_bytes is not None:
        if type(max_bytes) is not int:
            raise ValueError("max_bytes must be an integer or None.")
        if max_bytes < 0:
            raise ValueError("max_bytes must be greater than or equal to zero.")
    max_download_size = config.max_download_size
    if max_download_size is None:
        return max_bytes
    if max_bytes is None:
        return max_download_size
    return min(max_download_size, max_bytes)


def _check_download_headers(headers: Mapping[str, str], max_bytes: int | None) -> None:
    # Content-Length describes encoded data; the iterators yield decoded bytes.
    if max_bytes is None or headers.get("Content-Encoding", "identity").strip().lower() != "identity":
        return
    try:
        declared_size = int(headers["Content-Length"])
    except (KeyError, TypeError, ValueError):
        return
    if declared_size > max_bytes:
        raise SandboxResponseTooLargeError(f"File size exceeds limit of {max_bytes} bytes.")


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


def _validate_binary_stream(content: BinaryIO) -> None:
    """Reject text streams and objects that do not provide a read method."""
    if isinstance(content, (io.StringIO, io.TextIOBase)):
        raise TypeError("File content must be opened in binary mode.")
    if not callable(getattr(content, "read", None)):
        raise TypeError("File content must be bytes, str, or a binary file object.")


def _read_binary_chunk(content: BinaryIO) -> bytes:
    """Read one bounded chunk and enforce the BinaryIO return contract."""
    chunk = content.read(_STREAM_CHUNK_SIZE)
    if isinstance(chunk, str):
        raise TypeError("File content must be opened in binary mode.")
    if not isinstance(chunk, (bytes, bytearray, memoryview)):
        raise TypeError("Binary file objects must return bytes from read().")
    return bytes(chunk)


def _iter_binary_chunks(content: BinaryIO) -> Iterator[bytes]:
    while chunk := _read_binary_chunk(content):
        yield chunk


def _multipart_framing(path: str) -> tuple[bytes, bytes, str]:
    """Build the framing for a streaming, single-file multipart request."""
    boundary = secrets.token_hex(16)
    field = RequestField(name="file", data=b"", filename=path)
    field.make_multipart(content_disposition="form-data")
    prefix = (
        f"--{boundary}\r\n".encode()
        + field.render_headers().encode("utf-8")
    )
    suffix = f"\r\n--{boundary}--\r\n".encode()
    content_type = f"multipart/form-data; boundary={boundary}"
    return prefix, suffix, content_type


def _iter_multipart_body(
    prefix: bytes,
    content: BinaryIO,
    suffix: bytes,
) -> Iterator[bytes]:
    yield prefix
    yield from _iter_binary_chunks(content)
    yield suffix


def _sandboxd_files_endpoint(path: str) -> str:
    """Return the sandboxd REST path for a sandbox file path.

    Separators stay literal "/". A leading "/" is encoded as "%2F", so
    "/workspace/a.txt" stays absolute and sandboxd can strip its own root.
    Stripping the slash here would join that path under the root a second
    time. A ".." segment is rejected before normalization, so
    "dir/../a.txt" is an error instead of a rewrite to "a.txt". "." and
    empty segments, including a trailing slash, are dropped.
    """
    if any(part == ".." for part in path.split("/")):
        raise ValueError(f"path {path!r} escapes the sandbox root")
    normalized = posixpath.normpath(path)
    if normalized in ("", ".", "/"):
        return "v1/files/"
    absolute = normalized.startswith("/")
    body = normalized[1:] if absolute else normalized
    encoded = "/".join(
        urllib.parse.quote(part, safe="") for part in body.split("/")
    )
    if absolute:
        return "v1/files/%2F" + encoded
    return "v1/files/" + encoded


class _RuntimeProbe(Protocol):
    """The part of a sync or async connector that the endpoint helpers use."""

    def is_sandboxd(self) -> bool: ...


def _sandboxd_op_endpoint(
    connector: _RuntimeProbe,
    op: str,
    path: str,
    action: str = "",
    query: Mapping[str, str] | None = None,
) -> str:
    """Return the endpoint for a sandboxd-only file operation.

    Raises NotImplementedError on the legacy runtime, which has none of these
    endpoints, and ValueError for an empty path.
    """
    if not connector.is_sandboxd():
        raise NotImplementedError(
            f"{op}() is only supported by the sandboxd runtime; the legacy "
            f"python-runtime has no {op} endpoint"
        )
    if path == "":
        raise ValueError(f"{op}: path must not be empty")
    endpoint = _sandboxd_files_endpoint(path)
    if endpoint == "v1/files/":
        raise ValueError(f"{op}: path {path!r} names the sandbox root")
    endpoint += action
    if query:
        endpoint += "?" + urllib.parse.urlencode(query)
    return endpoint


def _mkdir_endpoint(connector: _RuntimeProbe, path: str, parents: bool) -> str:
    """Return the endpoint for a sandboxd directory creation."""
    query = {"type": "directory"}
    if parents:
        query["parents"] = "true"
    return _sandboxd_op_endpoint(connector, "mkdir", path, query=query)


def _transfer_endpoint(
    connector: _RuntimeProbe,
    op: str,
    source: str,
    destination: str,
    overwrite: bool,
) -> str:
    """Return the endpoint for a sandboxd move or copy."""
    if destination == "":
        raise ValueError(f"{op}: destination must not be empty")
    query = {"destination": destination}
    if overwrite:
        query["overwrite"] = "true"
    # The encoded path has no literal colon, so sandboxd splits the action
    # off at the last one.
    return _sandboxd_op_endpoint(connector, op, source, f":{op}", query)


class Filesystem:
    """
    Handles file operations within the sandbox.

    Speaks either the legacy python-runtime HTTP API or the sandboxd
    Filesystem & Runtime REST API, selected by the connection config
    (``connector.is_sandboxd()``).
    """
    def __init__(
        self, connector: SandboxConnector, tracer: Any, trace_service_name: str,
        *, file_config: SandboxFileConfig | None = None,
    ) -> None:
        self.connector = connector
        self.tracer = tracer
        self.trace_service_name = trace_service_name
        self._file_config = file_config or SandboxFileConfig()

    @trace_span("write")
    def write(
        self,
        path: str,
        content: bytes | str | BinaryIO,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,
    ) -> None:
        """Upload content to a sandbox-relative path.

        Binary file objects are read in chunks from their current position and
        sent once without retries. The caller must close the file object; its
        position may advance if the upload fails.
        """
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

        # The sandbox runtime uses the multipart ``filename`` field as a
        # relative destination path under its base directory (e.g. /app).
        # ``os.path.join`` on the server will honor absolute paths and
        # ``..`` segments, so a caller could otherwise escape the
        # confinement by sending filename='/etc/passwd' or '../etc/...'.
        # Sanitize here to guarantee the filename is a normalized
        # relative path with no upward traversal.
        if not allow_unsafe_paths:
            path = self._safe_upload_path(
                path, preserve_absolute=self.connector.is_sandboxd()
            )

        if self.connector.is_sandboxd():
            # sandboxd write is an idempotent PUT of the raw bytes; parent
            # directories are created server-side (temp-file + rename).
            data: bytes | Iterator[bytes]
            if isinstance(content, bytes):
                data = content
                disable_retries = False
            else:
                data = _iter_binary_chunks(content)
                disable_retries = True
            self.connector.send_request(
                "PUT", _sandboxd_files_endpoint(path),
                data=data,
                headers={"Content-Type": "application/octet-stream"},
                timeout=timeout,
                _disable_retries=disable_retries,
            )
        elif not isinstance(content, bytes):
            prefix, suffix, content_type = _multipart_framing(path)
            # POST is intentionally absent from RETRYABLE_METHODS. Making it
            # retryable would replay this consumed generator as an empty body.
            self.connector.send_request(
                "POST",
                "upload",
                data=_iter_multipart_body(prefix, content, suffix),
                headers={"Content-Type": content_type},
                timeout=timeout,
                _disable_retries=True,
            )
        else:
            files_payload = {'file': (path, content)}
            self.connector.send_request("POST", "upload",
                          files=files_payload, timeout=timeout)
        logger.info(f"File '{path}' uploaded successfully.")

    @staticmethod
    def _safe_upload_path(path: str, *, preserve_absolute: bool = False) -> str:
        """Return a ``..``-free path safe to send to the runtime.

        Rejects NUL bytes and ASCII control characters before normalisation:
        ``os.path.normpath`` preserves embedded NULs, and a NUL in the
        filename truncates at the runtime's C/syscall layer. Without this
        check ``foo\\x00../etc/passwd`` would survive the ``..`` split (no
        part equals ``".."`` because the NUL-prefixed segment doesn't
        match) yet resolve to ``foo`` on the filesystem — or worse,
        something server-dependent.

        A leading "/" is kept when ``preserve_absolute`` is set. sandboxd
        needs that slash so "/workspace/a.txt" is not joined under the
        workspace root a second time. The legacy multipart filename leaves
        it unset and strips the slash, because that field is a relative
        path and an absolute filename would escape the runtime directory.
        """
        if any(ord(c) < 0x20 or ord(c) == 0x7F for c in path):
            raise ValueError(
                f"Upload path contains ASCII control characters: {path!r}"
            )
        if not path.strip():
            raise ValueError("Upload path cannot be empty.")
        # Reject ".." before normpath. Otherwise "dir/../a.txt" is rewritten
        # to "a.txt" and the caller never sees that the path moved.
        if any(part == ".." for part in path.split("/")):
            raise ValueError(
                f"Upload path '{path}' escapes the sandbox root."
            )

        normalized = posixpath.normpath(path)
        if not preserve_absolute:
            normalized = normalized.lstrip("/")
        if normalized in ("", ".", "/"):
            raise ValueError(f"Upload path '{path}' does not name a file.")
        return normalized

    @trace_span("read")
    def read(
        self,
        path: str,
        timeout: int = 60,
        allow_unsafe_paths: bool = False,

    ) -> bytes:
        """Read a sandbox-relative file and return its complete raw bytes.

        A configured ``max_download_size`` is enforced while downloading;
        exceeding it raises ``SandboxResponseTooLargeError`` without returning
        partial content. Downloads remain unlimited when it is unset.
        """
        if self._file_config.max_download_size is not None:
            with io.BytesIO() as destination:
                self._read_to(path, destination, timeout, allow_unsafe_paths, None)
                return destination.getvalue()

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        if not allow_unsafe_paths:
            path = self._safe_upload_path(
                path, preserve_absolute=self.connector.is_sandboxd()
            )

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

        The destination is never closed. The smaller of ``max_bytes`` and the
        configured ``max_download_size`` applies, ignoring unset limits.
        At most that many bytes are written before an oversized download raises
        ``SandboxResponseTooLargeError`` (a ``RuntimeError`` subclass). Data
        written before an error remains in the destination. The returned value
        is the number of bytes written.
        """
        return self._read_to(path, destination, timeout, allow_unsafe_paths, max_bytes)

    def _read_to(
        self, path: str, destination: BinaryWriter, timeout: int,
        allow_unsafe_paths: bool, max_bytes: int | None,
    ) -> int:
        if destination is None or not callable(getattr(destination, "write", None)):
            raise TypeError("Download destination must provide a write(bytes) method.")
        max_bytes = _download_limit(self._file_config, max_bytes)

        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)

        if not allow_unsafe_paths:
            path = self._safe_upload_path(
                path, preserve_absolute=self.connector.is_sandboxd()
            )

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
            _check_download_headers(response.headers, max_bytes)

            for chunk in response.iter_content(chunk_size=_STREAM_CHUNK_SIZE):
                if not chunk:
                    continue
                if max_bytes is not None:
                    remaining = max_bytes - total
                    if len(chunk) > remaining:
                        if remaining > 0:
                            total += _write_all(destination, chunk[:remaining])
                        raise SandboxResponseTooLargeError(
                            f"File size exceeds limit of {max_bytes} bytes."
                        )
                total += _write_all(destination, chunk)
        except requests.exceptions.RequestException:
            transport_token = getattr(response, "_sandboxd_transport_token", None)
            if transport_token is not None:
                self.connector.invalidate_sandboxd_transport(
                    None, transport_token=transport_token
                )
            raise
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
                    logger.info(f"Skipping unsupported file entry type: {entry.get('type')!r}")
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
        return bool(exists)

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
        endpoint = _sandboxd_op_endpoint(
            self.connector, "delete", path,
            query={"recursive": "true"} if recursive else None)
        self.connector.send_request("DELETE", endpoint, timeout=timeout)
        logger.info(f"Path '{path}' deleted successfully.")

    @trace_span("mkdir")
    def mkdir(self, path: str, parents: bool = False, timeout: int = 60) -> None:
        """Create a directory. sandboxd runtime only.

        Succeeds if the directory already exists. With ``parents=False`` a
        missing parent fails with a 404. The legacy python-runtime raises
        NotImplementedError.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", path)
        endpoint = _mkdir_endpoint(self.connector, path, parents)
        self.connector.send_request("PUT", endpoint, timeout=timeout)
        logger.info(f"Directory '{path}' created successfully.")

    @trace_span("move")
    def move(
        self, source: str, destination: str, overwrite: bool = False, timeout: int = 60
    ) -> None:
        """Rename a file or directory. sandboxd runtime only.

        Missing parents of ``destination`` are created. An existing destination
        fails with a 409 unless ``overwrite=True``, which replaces a file but
        never a directory. The legacy python-runtime raises
        NotImplementedError.
        """
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", source)
        endpoint = _transfer_endpoint(
            self.connector, "move", source, destination, overwrite)
        self.connector.send_request("POST", endpoint, timeout=timeout)
        logger.info(f"Path '{source}' moved to '{destination}'.")

    @trace_span("copy")
    def copy(
        self, source: str, destination: str, overwrite: bool = False, timeout: int = 60
    ) -> None:
        """Copy a file or directory with the same rules as ``move()``."""
        span = trace.get_current_span()
        if span.is_recording():
            span.set_attribute("sandbox.file.path", source)
        endpoint = _transfer_endpoint(
            self.connector, "copy", source, destination, overwrite)
        self.connector.send_request("POST", endpoint, timeout=timeout)
        logger.info(f"Path '{source}' copied to '{destination}'.")
