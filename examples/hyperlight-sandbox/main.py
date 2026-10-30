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

"""The python-runtime-sandbox API, served from inside a Hyperlight micro-VM.

This speaks the same HTTP API as ../python-runtime-sandbox/main.py (GET /,
POST /execute, POST /upload, GET /download, GET /list, GET /exists), so the
k8s-agent-sandbox SDK drives it unchanged. It is written against the standard
library only because the guest is a Unikraft unikernel booted from a rootfs
CPIO: the published hyperlight-unikraft `agent` rootfs already carries
CPython, a BusyBox shell and the data-science stack, and keeping FastAPI and
uvicorn out of it keeps the rootfs, and the warm snapshot taken of it, small.
"""

import email.message
import email.parser
import email.policy
import email.utils
import http.server
import json
import logging
import math
import os
import re
import signal
import socketserver
import shutil
import subprocess
import sys
import tempfile
import urllib.parse

# Exit code reported when a command is killed for exceeding its timeout,
# matching ../python-runtime-sandbox/main.py.
TIMEOUT_EXIT_CODE = 124
PORT = 8888
# The runtime reads and writes request bodies in pieces of this size.
# Uploaded files live in guest memory, so it never holds a body whole on
# top of that.
CHUNK_SIZE = 64 * 1024
# How far ahead of what has arrived an upload is allocated; see GrowingFile.
UPLOAD_GROW_STEP = 32 * 1024 * 1024
# How long a connection may stay silent, mid-request or between requests,
# before it is dropped along with any upload it was sending.
CONNECTION_TIMEOUT_SECONDS = 30
# Where uploads in progress are written, a hidden directory of the base
# directory (the same filesystem, so a finished upload is renamed into place).
# The API does not show it or let a request reach into it.
UPLOADS_IN_PROGRESS = ".sandbox-uploads"
# The largest /execute request body accepted.
MAX_EXECUTE_BODY = 1024 * 1024
# The largest part headers and preamble of a multipart upload accepted.
MAX_MULTIPART_HEADER = 16 * 1024
# The most trailer bytes a chunked body may end with.
MAX_CHUNKED_TRAILERS = 16 * 1024
# A refused body is read and discarded before the answer, so that the
# client gets the answer rather than a reset, up to this many times the
# limit it was refused for.
DRAIN_FACTOR = 4


class ClientGone(Exception):
    """The client closed or reset the connection, or went silent too long."""


class BadRequest(Exception):
    """A request the runtime refuses, answered with this status and detail.

    The detail is sent under key, "detail" as FastAPI names it, or
    "message" where the python-runtime-sandbox API does.
    """

    def __init__(self, status: int, detail: str, key: str = "detail"):
        super().__init__(detail)
        self.status = status
        self.detail = detail
        self.key = key


def get_base_dir() -> str:
    return os.environ.get("SANDBOX_BASE_DIR", "").strip() or "/app"


def get_safe_path(file_path: str) -> str:
    """Confines file_path to the base directory, as the FastAPI runtime does."""
    base_dir = os.path.realpath(get_base_dir())
    clean_path = file_path.lstrip("/")
    full_path = os.path.realpath(os.path.join(base_dir, clean_path))
    if os.path.commonpath([base_dir, full_path]) != base_dir:
        raise ValueError("Access denied: Path must be within the sandbox base directory")
    return full_path


def is_in_uploads_dir(full_path: str) -> bool:
    uploads_dir = os.path.join(os.path.realpath(get_base_dir()), UPLOADS_IN_PROGRESS)
    return full_path == uploads_dir or full_path.startswith(uploads_dir + os.sep)


def get_exec_timeout_seconds() -> float:
    raw = os.environ.get("SANDBOX_EXEC_TIMEOUT_SECONDS", "300")
    try:
        value = float(raw)
    except ValueError:
        value = float("nan")
    if not (math.isfinite(value) and value > 0):
        logging.warning("Ignoring invalid SANDBOX_EXEC_TIMEOUT_SECONDS=%r; using 300 seconds", raw)
        return 300.0
    return value


def get_max_upload_bytes() -> int:
    """Reads SANDBOX_MAX_UPLOAD_BYTES, the largest upload body accepted.

    An uploaded file is kept in guest memory, which the runtime and every
    command share. The default leaves room for a few uploads.
    """
    raw = os.environ.get("SANDBOX_MAX_UPLOAD_BYTES", "")
    try:
        value = int(raw)
    except ValueError:
        value = 0
    if value <= 0:
        if raw:
            logging.warning("Ignoring invalid SANDBOX_MAX_UPLOAD_BYTES=%r", raw)
        return 100 * 1024 * 1024
    return value


class BodyReader:
    """Reads a request body framed by Content-Length or chunked encoding.

    It never returns more than limit bytes in total and raises BadRequest
    (413) past it, so a body is refused before it can fill guest memory.
    """

    def __init__(self, rfile, headers, limit: int):
        self._rfile = rfile
        self._limit = limit
        self._total = 0
        # Set once the framing was found invalid: where the body ends is
        # then unknown, so the connection cannot be reused.
        self._broken = False
        encoding = (headers.get("Transfer-Encoding") or "").strip().lower()
        length = headers.get("Content-Length")
        if encoding:
            if encoding != "chunked":
                raise BadRequest(501, f"Unsupported Transfer-Encoding: {encoding}")
            self._chunked = True
            self._chunk_left = 0
            self._done = False
            return
        self._chunked = False
        if length is None:
            self._left = 0
            return
        if not re.fullmatch(r"[0-9]+", length.strip(" \t")):
            raise BadRequest(400, "Invalid Content-Length")
        self._left = int(length)

    def declared_size(self):
        """The Content-Length of the body, None for a chunked one."""
        return None if self._chunked else self._total + self._left

    def read(self, size: int = CHUNK_SIZE) -> bytes:
        """Returns up to size bytes of the body, b"" at its end."""
        if not self._chunked and self._total + self._left > self._limit:
            raise BadRequest(413, f"Request body larger than {self._limit} bytes")
        data = self._read_chunked(size) if self._chunked else self._read_plain(size)
        self._total += len(data)
        if self._total > self._limit:
            raise BadRequest(413, f"Request body larger than {self._limit} bytes")
        return data

    def read_all(self) -> bytes:
        parts = []
        while chunk := self.read():
            parts.append(chunk)
        return b"".join(parts)

    def drain(self) -> bool:
        """Reads and discards the rest of the body, up to DRAIN_FACTOR times the limit.

        Returns whether the body was read to its end. A client that is
        still sending when the socket closes under it sees a reset rather
        than the answer, so a refused body is read to its end first. The
        rest of a body whose framing was invalid cannot be read, and is not.
        """
        if self._broken:
            return False
        left = DRAIN_FACTOR * self._limit
        try:
            while left > 0:
                size = min(CHUNK_SIZE, left)
                data = self._read_chunked(size) if self._chunked else self._read_plain(size)
                if not data:
                    break
                left -= len(data)
        except (BadRequest, ClientGone):
            return False
        return self._done if self._chunked else self._left == 0

    def _socket(self, read, *args) -> bytes:
        try:
            return read(*args)
        except OSError as e:
            raise ClientGone(str(e)) from None

    def _read_plain(self, size: int) -> bytes:
        if self._left == 0:
            return b""
        data = self._socket(self._rfile.read, min(size, self._left))
        if not data:
            raise ClientGone("connection closed mid-body")
        self._left -= len(data)
        return data

    def _read_chunked(self, size: int) -> bytes:
        if self._done:
            return b""
        if self._chunk_left == 0:
            line = self._socket(self._rfile.readline, 1024)
            # chunk-size is hex digits only (RFC 9112), then any extensions.
            size_field = line.rstrip(b"\r\n").split(b";", 1)[0].strip(b" \t")
            if not line.endswith(b"\n") or not re.fullmatch(rb"[0-9A-Fa-f]{1,15}", size_field):
                raise self._invalid("Invalid chunk size")
            self._chunk_left = int(size_field, 16)
            if self._chunk_left == 0:
                # Skip any trailers, up to the blank line that ends the body.
                budget = MAX_CHUNKED_TRAILERS
                while (line := self._socket(self._rfile.readline, 1024)) not in (b"\r\n", b"\n"):
                    if not line:
                        raise ClientGone("connection closed mid-body")
                    budget -= len(line)
                    if budget < 0:
                        raise self._invalid("Chunked trailers too large")
                self._done = True
                return b""
        data = self._socket(self._rfile.read, min(size, self._chunk_left))
        if not data:
            raise ClientGone("connection closed mid-body")
        self._chunk_left -= len(data)
        if self._chunk_left == 0 and self._socket(self._rfile.readline, 1024) not in (b"\r\n", b"\n"):
            raise self._invalid("Invalid chunk framing")
        return data

    def _invalid(self, detail: str) -> BadRequest:
        self._broken = True
        return BadRequest(400, detail)


class GrowingFile:
    """Writes a file whose space is allocated in steps ahead of the writes.

    The guest's in-memory filesystem grows a file by half at least when a
    write runs past its end, and copies it each time, holding both copies
    for a moment. Allocating ahead keeps those copies few. The file grows by
    UPLOAD_GROW_STEP or, when the body's size is known, by doubling what has
    arrived up to that size, so the last step holds one and a half times
    the file rather than two. It is never allocated more than twice what
    has arrived plus a step, so a client that declares a large body and
    stalls holds little more memory than it has sent. The file is cut to
    what was written at the end.
    """

    def __init__(self, fd: int, expected_size=None):
        self._fd = fd
        self._expected = expected_size
        self._written = 0
        self._allocated = 0

    def write(self, data: bytes) -> None:
        end = self._written + len(data)
        if end > self._allocated:
            self._allocated = end + UPLOAD_GROW_STEP
            if self._expected and self._expected > end:
                self._allocated = min(self._expected, max(self._allocated, 2 * end))
            os.ftruncate(self._fd, self._allocated)
        view = memoryview(data)
        while view:
            n = os.write(self._fd, view)
            view = view[n:]
        self._written = end

    def finish(self) -> None:
        os.ftruncate(self._fd, self._written)


class MultipartStream:
    """A minimal streaming reader for a multipart/form-data body."""

    def __init__(self, body: BodyReader, boundary: bytes):
        self._body = body
        # A delimiter stands at the start of a line, and the body's first
        # line is one; so the first delimiter is found as the others are.
        self._buf = b"\r\n"
        self._separator = b"\r\n--" + boundary

    def _fill(self) -> None:
        chunk = self._body.read()
        if not chunk:
            raise BadRequest(400, "Multipart body ended early")
        self._buf += chunk

    def _read_until(self, token: bytes, limit: int) -> bytes:
        while (i := self._buf.find(token)) < 0:
            if len(self._buf) > limit:
                raise BadRequest(400, "Multipart headers too large")
            self._fill()
        if i > limit:
            raise BadRequest(400, "Multipart headers too large")
        data, self._buf = self._buf[:i], self._buf[i + len(token):]
        return data

    def _copy_until(self, token: bytes, write) -> None:
        # Holds back len(token) - 1 bytes, the most of a token that can be
        # cut off at the end of what has been read so far.
        keep = len(token) - 1
        while (i := self._buf.find(token)) < 0:
            if len(self._buf) > keep:
                write(self._buf[:-keep])
                self._buf = self._buf[-keep:]
            self._fill()
        write(self._buf[:i])
        self._buf = self._buf[i + len(token):]

    def parts(self):
        """Yields (headers, copy) per part; copy(write) streams its content.

        A part's content must be copied, or skipped with copy(None), before
        the next part is read.
        """
        self._read_until(self._separator, MAX_MULTIPART_HEADER)
        while True:
            while len(self._buf) < 2:
                self._fill()
            if self._buf[:2] == b"--":
                # The closing delimiter; drop whatever epilogue follows.
                while self._body.read():
                    pass
                return
            # Whitespace may pad the delimiter's line (RFC 2046).
            padding = 0
            while self._buf[:1] in (b" ", b"\t"):
                self._buf = self._buf[1:]
                padding += 1
                if padding > MAX_MULTIPART_HEADER:
                    raise BadRequest(400, "Invalid multipart delimiter")
                while len(self._buf) < 2:
                    self._fill()
            marker, self._buf = self._buf[:2], self._buf[2:]
            if marker != b"\r\n":
                raise BadRequest(400, "Invalid multipart delimiter")
            while len(self._buf) < 2:
                self._fill()
            if self._buf.startswith(b"\r\n"):
                # A part with no headers at all: its blank line comes first.
                raw, self._buf = b"", self._buf[2:]
            else:
                raw = self._read_until(b"\r\n\r\n", MAX_MULTIPART_HEADER)
            # compat32 unquotes parameters as python-multipart does, undoing
            # only \" and \\, so other backslashes stay in a filename.
            # Clients send a non-ASCII filename as raw UTF-8.
            headers = email.parser.Parser(policy=email.policy.compat32).parsestr(
                raw.decode("utf-8", "surrogateescape") + "\r\n\r\n"
            )
            yield headers, lambda write: self._copy_until(self._separator, write or (lambda _: None))


def run_command(command: str, timeout: float) -> dict:
    # The command leads a session of its own so that a timeout kills
    # everything it started, not just the shell, as the FastAPI runtime does.
    with subprocess.Popen(
        ["/bin/sh", "-c", command],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        cwd=get_base_dir(),
        start_new_session=True,
    ) as process:
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except OSError:
                pass
            stdout, stderr = process.communicate()
            if stderr and not stderr.endswith("\n"):
                stderr += "\n"
            return {
                "stdout": stdout or "",
                "stderr": f"{stderr or ''}Command timed out after {timeout:g} seconds",
                "exit_code": TIMEOUT_EXIT_CODE,
                "timed_out": True,
            }
        return {"stdout": stdout, "stderr": stderr, "exit_code": process.returncode, "timed_out": False}


class Handler(http.server.BaseHTTPRequestHandler):
    # Keep-alive lets the SDK reuse one connection, which matters more than
    # usual here: every guest socket call is a VM exit.
    protocol_version = "HTTP/1.1"
    timeout = CONNECTION_TIMEOUT_SECONDS

    def log_error(self, format, *args):
        # A keep-alive connection that went idle past the timeout is
        # closed without a word: the SDK holds one open between calls,
        # and each log line is a call out of the micro-VM.
        if format.startswith("Request timed out"):
            return
        super().log_error(format, *args)

    def log_request(self, code="-", size="-"):
        # The readiness probe's GET / comes every second, and each log line
        # is a call out of the micro-VM, so a probe that passed is not logged.
        if self.command == "GET" and self.path == "/" and code == 200:
            return
        super().log_request(code, size)

    def parse_request(self) -> bool:
        # Set when the request's body is left unread. The next request on
        # this connection would read it as its own, so the connection ends
        # with the response instead.
        self._close = False
        if not super().parse_request():
            return False
        if self.command != "POST" and (
            self.headers.get("Transfer-Encoding") or self.headers.get("Content-Length", "0").strip() not in ("", "0")
        ):
            self._close = True
        return True

    def end_headers(self) -> None:
        if getattr(self, "_close", False) and not self.close_connection:
            self.send_header("Connection", "close")
        super().end_headers()

    def _send(self, status: int, body: bytes, content_type: str, extra_headers=None, close=False) -> None:
        self._close = self._close or close
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        for key, value in (extra_headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        # In pieces, as downloads are. The socket timeout bounds each write,
        # and one write of a large reply to a slow client would be cut off.
        view = memoryview(body)
        for start in range(0, len(view), CHUNK_SIZE):
            self.wfile.write(view[start:start + CHUNK_SIZE])

    def _json(self, status: int, payload, close=False) -> None:
        self._send(status, json.dumps(payload).encode(), "application/json", close=close)

    def _route(self, prefix: str):
        """Returns the sandbox path after prefix, or None when the request is not for it.

        The SDK quotes the whole path, slashes included, into one segment.
        """
        path = urllib.parse.urlsplit(self.path).path
        if not path.startswith(prefix):
            return None
        return urllib.parse.unquote(path[len(prefix):])

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == "/":
            return self._json(200, {"status": "ok", "message": "Sandbox Runtime is active."})
        for prefix, handler in (
            ("/download/", self._download),
            ("/list/", self._list),
            ("/exists/", self._exists),
        ):
            target = self._route(prefix)
            if target is not None:
                try:
                    full_path = get_safe_path(target)
                except ValueError:
                    return self._json(403, {"message": "Access denied"})
                return handler(target, full_path)
        self._json(404, {"detail": "Not Found"})

    def do_POST(self):
        path = urllib.parse.urlsplit(self.path).path
        body = None
        try:
            if path == "/execute":
                body = BodyReader(self.rfile, self.headers, MAX_EXECUTE_BODY)
                return self._execute(body)
            if path == "/upload":
                body = BodyReader(self.rfile, self.headers, get_max_upload_bytes())
                return self._upload(body)
        except BadRequest as e:
            # What the client is still sending is read first, so that it
            # gets this answer; see BodyReader.drain.
            drained = body is not None and body.drain()
            return self._json(e.status, {e.key: e.detail}, close=not drained)
        except ClientGone as e:
            # Nobody is left to answer.
            logging.info("Client left during %s: %s", path, e)
            self.close_connection = True
            return
        self._json(404, {"detail": "Not Found"}, close=True)

    def _execute(self, body: BodyReader):
        data = body.read_all()
        try:
            request = json.loads(data)
            if not isinstance(request, dict) or not isinstance(request.get("command"), str):
                return self._json(422, {"detail": "Expected a JSON object with a string 'command'"})
            command = request["command"]
            timeout = get_exec_timeout_seconds()
            requested = request.get("timeout_seconds")
            if isinstance(requested, str):
                # A number in a string is taken, as pydantic takes it for
                # the FastAPI runtime.
                try:
                    requested = float(requested)
                except ValueError:
                    return self._json(422, {"detail": "timeout_seconds must be a finite number greater than 0"})
            if requested is not None:
                if isinstance(requested, bool) or not (
                    isinstance(requested, (int, float)) and math.isfinite(requested) and requested > 0
                ):
                    return self._json(422, {"detail": "timeout_seconds must be a finite number greater than 0"})
                timeout = min(timeout, requested)
        except (ValueError, KeyError, TypeError) as e:
            return self._json(422, {"detail": f"Invalid request: {e}"})
        try:
            result = run_command(command, timeout)
        except Exception as e:
            result = {"stdout": "", "stderr": f"Failed to execute command: {e}", "exit_code": 1, "timed_out": False}
        self._json(200, result)

    def _upload(self, body: BodyReader):
        content_type = email.message.EmailMessage()
        content_type["Content-Type"] = self.headers.get("Content-Type", "")
        boundary = content_type.get_param("boundary")
        if isinstance(boundary, tuple):  # an RFC 2231 parameter (charset, language, value)
            boundary = email.utils.collapse_rfc2231_value(boundary)
        if content_type.get_content_type() != "multipart/form-data" or not boundary:
            raise BadRequest(422, "Expected a multipart/form-data body")
        base_dir = os.path.realpath(get_base_dir())
        uploads_dir = os.path.join(base_dir, UPLOADS_IN_PROGRESS)
        # Temporary files not yet renamed into place, removed however the
        # request ends. The last is the "file" part to keep: like the FastAPI
        # runtime, a later "file" part replaces an earlier one.
        temps = []
        try:
            for headers, copy in MultipartStream(body, boundary.encode()).parts():
                if headers.get_param("name", header="content-disposition") != "file":
                    copy(None)
                    continue
                filename = headers.get_filename()
                if filename:
                    filename = filename.encode("utf-8", "surrogateescape").decode("utf-8", "replace")
                if not filename:
                    raise BadRequest(422, "Expected a multipart 'file' field")
                try:
                    file_path = get_safe_path(filename)
                except ValueError:
                    raise BadRequest(403, "Access denied", key="message") from None
                if file_path == base_dir or is_in_uploads_dir(file_path) or os.path.isdir(file_path):
                    raise BadRequest(400, f"Invalid filename: {filename!r}")
                try:
                    # An earlier "file" part is replaced by this one. One
                    # that cannot be removed stays listed, for the cleanup.
                    while temps:
                        os.unlink(temps[-1][0])
                        temps.pop()
                    os.makedirs(uploads_dir, exist_ok=True)
                    fd, tmp_path = tempfile.mkstemp(dir=uploads_dir)
                    temps.append((tmp_path, file_path, filename))
                    try:
                        # mkstemp creates the file private; give it open()'s mode.
                        os.fchmod(fd, 0o644)
                        out = GrowingFile(fd, body.declared_size())
                        copy(out.write)
                        out.finish()
                    finally:
                        os.close(fd)
                except OSError as e:
                    raise BadRequest(500, f"File upload failed: {e}", key="message") from None
            # Only a body read whole and well-formed puts a file in place.
            if not temps:
                raise BadRequest(422, "Expected a multipart 'file' field")
            tmp_path, file_path, filename = temps.pop()
            try:
                os.makedirs(os.path.dirname(file_path), exist_ok=True)
                os.replace(tmp_path, file_path)
            except OSError as e:
                temps.append((tmp_path, file_path, filename))
                return self._json(500, {"message": f"File upload failed: {e}"})
        finally:
            for tmp_path, _, _ in temps:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
        self._json(200, {"message": f"File '{filename}' uploaded successfully."})

    def _download(self, _target: str, full_path: str):
        if not os.path.isfile(full_path) or is_in_uploads_dir(full_path):
            return self._json(404, {"message": "File not found"})
        name = urllib.parse.quote(os.path.basename(full_path))
        try:
            f = open(full_path, "rb")
        except OSError as e:
            return self._json(500, {"message": f"Cannot read file: {e.strerror}"})
        with f:
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(os.fstat(f.fileno()).st_size))
            self.send_header("Content-Disposition", f"attachment; filename*=utf-8''{name}")
            self.end_headers()
            shutil.copyfileobj(f, self.wfile, CHUNK_SIZE)

    def _list(self, _target: str, full_path: str):
        if not os.path.isdir(full_path) or is_in_uploads_dir(full_path):
            return self._json(404, {"message": "Path is not a directory"})
        # The uploads directory sits right under the base directory.
        hide_uploads = full_path == os.path.realpath(get_base_dir())
        try:
            entries = []
            with os.scandir(full_path) as it:
                for entry in it:
                    if hide_uploads and entry.name == UPLOADS_IN_PROGRESS:
                        continue
                    size, mod_time = 0, 0.0
                    for follow_symlinks in (True, False):
                        try:
                            stats = entry.stat(follow_symlinks=follow_symlinks)
                            size, mod_time = stats.st_size, stats.st_mtime
                            break
                        except OSError:
                            # A symbolic link to nothing is listed as itself.
                            # The guest kernel cannot stat even the link, so
                            # it is listed with no size.
                            continue
                    entries.append({
                        "name": entry.name,
                        "size": size,
                        "type": "directory" if entry.is_dir() else "file",
                        "mod_time": mod_time,
                    })
        except OSError as e:
            return self._json(500, {"message": f"List files failed: {e}"})
        self._json(200, entries)

    def _exists(self, target: str, full_path: str):
        exists = os.path.exists(full_path) and not is_in_uploads_dir(full_path)
        self._json(200, {"path": target, "exists": exists})


class Server(http.server.ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        # A client that left while its answer was being written is one
        # log line, not a traceback. The SDK cuts a slow download short.
        error = sys.exc_info()[1]
        if isinstance(error, (BrokenPipeError, ConnectionResetError)):
            logging.info("Client %s left mid-response: %s", client_address[0], error)
            return
        super().handle_error(request, client_address)

    def server_bind(self):
        # HTTPServer resolves the host's FQDN here, for CGI's SERVER_NAME
        # only. That lookup is the slowest part of starting the runtime from
        # a snapshot (~40 ms), so it is skipped.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    os.makedirs(get_base_dir(), exist_ok=True)
    # A thread per connection, so a long /execute does not hold up the
    # health check or the file endpoints.
    server = Server(("0.0.0.0", PORT), Handler)
    logging.info("Sandbox runtime listening on :%d", PORT)
    server.serve_forever()


if __name__ == "__main__":
    main()
