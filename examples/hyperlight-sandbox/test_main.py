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

"""Tests for the runtime in main.py, run with `python3 -m pytest test_main.py`.

The runtime serves on a loopback port here, as it does in the guest, and
the tests speak HTTP to it with the standard library. The body framing,
the multipart parser and the path checks are the parts worth testing: the
runtime implements them itself, where the python-runtime-sandbox example
has FastAPI do it.
"""

import http.client
import io
import json
import logging
import os
import socket
import threading
import time
from unittest.mock import patch

import pytest

import main
from main import (
    BadRequest,
    BodyReader,
    GrowingFile,
    Handler,
    Server,
    UPLOADS_IN_PROGRESS,
    get_safe_path,
    is_in_uploads_dir,
)


@pytest.fixture
def base_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("SANDBOX_BASE_DIR", str(tmp_path))
    return tmp_path


@pytest.fixture
def server(base_dir, monkeypatch):
    # A connection that goes silent is dropped sooner than in production,
    # so a test that leaves one behind does not hold the server up.
    monkeypatch.setattr(Handler, "timeout", 2)
    srv = Server(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    yield srv
    srv.shutdown()
    srv.server_close()
    thread.join(timeout=5)


def connect(server) -> http.client.HTTPConnection:
    return http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)


def request(server, method, path, body=None, headers=None):
    """Sends one request on a fresh connection and returns (response, body bytes)."""
    conn = connect(server)
    try:
        conn.request(method, path, body=body, headers=headers or {})
        response = conn.getresponse()
        return response, response.read()
    finally:
        conn.close()


def raw(server, data: bytes):
    """Sends data as is and returns the parsed response and its body.

    For requests http.client would not send: invalid framing, chunked
    bodies with trailers, and the like.
    """
    with socket.create_connection(("127.0.0.1", server.server_address[1]), timeout=5) as sock:
        sock.sendall(data)
        response = http.client.HTTPResponse(sock, method="POST")
        response.begin()
        return response, response.read()


def execute(server, command, **fields):
    body = json.dumps({"command": command, **fields}).encode()
    response, data = request(server, "POST", "/execute", body, {"Content-Type": "application/json"})
    return response.status, json.loads(data)


def multipart(parts, boundary="b0undary"):
    """Builds a multipart/form-data body from (name, filename, content) parts."""
    out = b""
    for name, filename, content in parts:
        out += f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"'.encode()
        if filename is not None:
            out += f'; filename="{filename}"'.encode()
        out += b"\r\n\r\n" + content + b"\r\n"
    return out + f"--{boundary}--\r\n".encode()


def upload(server, body, content_type="multipart/form-data; boundary=b0undary"):
    response, data = request(server, "POST", "/upload", body, {"Content-Type": content_type})
    return response.status, json.loads(data)


def test_health_check(server):
    response, data = request(server, "GET", "/")
    assert response.status == 200
    assert json.loads(data) == {"status": "ok", "message": "Sandbox Runtime is active."}


def test_unknown_paths(server):
    assert request(server, "GET", "/nothing")[0].status == 404
    assert request(server, "POST", "/nothing", b"{}")[0].status == 404


class TestGetSafePath:
    def test_allows_relative_path_within_base(self, base_dir):
        assert get_safe_path("foo/bar.txt") == os.path.join(os.path.realpath(base_dir), "foo", "bar.txt")

    def test_strips_leading_slashes(self, base_dir):
        assert get_safe_path("///foo.txt") == os.path.join(os.path.realpath(base_dir), "foo.txt")

    def test_rejects_parent_directory_traversal(self, base_dir):
        with pytest.raises(ValueError):
            get_safe_path("../../etc/passwd")

    def test_rejects_traversal_hidden_inside_path(self, base_dir):
        with pytest.raises(ValueError):
            get_safe_path("foo/../../bar")

    def test_rejects_a_symlink_out_of_the_base(self, base_dir):
        os.symlink("/", base_dir / "root")
        with pytest.raises(ValueError):
            get_safe_path("root/etc/passwd")

    def test_defaults_to_app(self):
        with patch.dict(os.environ, {"SANDBOX_BASE_DIR": ""}):
            assert get_safe_path("x") == os.path.join(os.path.realpath("/app"), "x")


def test_is_in_uploads_dir(base_dir):
    uploads = os.path.join(os.path.realpath(base_dir), UPLOADS_IN_PROGRESS)
    assert is_in_uploads_dir(uploads)
    assert is_in_uploads_dir(os.path.join(uploads, "tmp123"))
    assert not is_in_uploads_dir(uploads + "-not")
    assert not is_in_uploads_dir(os.path.realpath(base_dir))


class TestExecute:
    def test_runs_a_command_in_the_base_dir(self, server, base_dir):
        status, result = execute(server, "pwd; echo out; echo err >&2; exit 3")
        assert status == 200
        assert result == {
            "stdout": f"{os.path.realpath(base_dir)}\nout\n",
            "stderr": "err\n",
            "exit_code": 3,
            "timed_out": False,
        }

    def test_a_timeout_kills_the_command_and_its_children(self, server):
        started = time.monotonic()
        status, result = execute(server, "echo started; sleep 30; echo late", timeout_seconds=0.5)
        assert status == 200
        assert time.monotonic() - started < 10
        assert result["timed_out"] is True
        assert result["exit_code"] == main.TIMEOUT_EXIT_CODE
        assert result["stdout"] == "started\n"
        assert result["stderr"].endswith("Command timed out after 0.5 seconds")

    def test_a_timeout_in_a_string_is_taken(self, server):
        # As the FastAPI runtime takes it, through pydantic.
        status, result = execute(server, "sleep 30", timeout_seconds="0.5")
        assert status == 200
        assert result["timed_out"] is True

    def test_the_configured_timeout_caps_the_requested_one(self, server, monkeypatch):
        monkeypatch.setenv("SANDBOX_EXEC_TIMEOUT_SECONDS", "0.5")
        status, result = execute(server, "sleep 30", timeout_seconds=600)
        assert result["timed_out"] is True
        assert result["stderr"].endswith("after 0.5 seconds")

    @pytest.mark.parametrize("body", [b"not json", b"[]", b'{"command": 5}', b'{"command": "x", "timeout_seconds": -1}',
                                      b'{"command": "x", "timeout_seconds": true}', b'{"command": "x", "timeout_seconds": "soon"}',
                                      b'{"command": "x", "timeout_seconds": "nan"}'])
    def test_rejects_a_bad_request(self, server, body):
        response, data = request(server, "POST", "/execute", body, {"Content-Type": "application/json"})
        assert response.status == 422
        assert "detail" in json.loads(data)

    def test_a_body_over_the_limit_is_refused_after_it_is_read(self, server):
        # http.client sends the whole body before it reads the answer, so
        # an answer sent while the body is still coming would be lost to
        # a reset: the runtime reads the body out first.
        body = b'{"command": "' + b"x" * (2 * main.MAX_EXECUTE_BODY) + b'"}'
        response, data = request(server, "POST", "/execute", body, {"Content-Type": "application/json"})
        assert response.status == 413
        assert json.loads(data) == {"detail": f"Request body larger than {main.MAX_EXECUTE_BODY} bytes"}

    def test_a_chunked_body_with_extensions_and_trailers(self, server):
        body = json.dumps({"command": "echo chunked"}).encode()
        head, tail = body[:7], body[7:]
        data = (
            b"POST /execute HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n"
            + f"{len(head):x};name=value\r\n".encode() + head + b"\r\n"
            + f"{len(tail):X}\r\n".encode() + tail + b"\r\n"
            + b"0\r\nX-Trailer: 1\r\n\r\n"
        )
        response, data = raw(server, data)
        assert response.status == 200
        assert json.loads(data)["stdout"] == "chunked\n"

    @pytest.mark.parametrize("framing, detail", [
        (b"zz\r\n{}\r\n0\r\n\r\n", "Invalid chunk size"),
        (b"2\r\n{}XX\r\n0\r\n\r\n", "Invalid chunk framing"),
        (b"0\r\n" + b"X-Long: " + b"y" * 1000 + b"\r\n" * 1 + (b"X: y\r\n" * 4000) + b"\r\n", "Chunked trailers too large"),
    ])
    def test_rejects_bad_chunked_framing(self, server, framing, detail):
        data = b"POST /execute HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n" + framing
        response, body = raw(server, data)
        assert response.status == 400
        assert json.loads(body) == {"detail": detail}
        # Where the body ends is unknown, so the connection is not reused.
        assert response.getheader("Connection") == "close"

    def test_rejects_an_invalid_content_length(self, server):
        response, body = raw(server, b"POST /execute HTTP/1.1\r\nHost: x\r\nContent-Length: abc\r\n\r\n")
        assert response.status == 400
        assert json.loads(body) == {"detail": "Invalid Content-Length"}

    def test_rejects_an_unsupported_transfer_encoding(self, server):
        response, body = raw(server, b"POST /execute HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: gzip\r\n\r\n")
        assert response.status == 501


class TestBodyReader:
    @staticmethod
    def reader(body: bytes, headers: dict, limit=1 << 20) -> BodyReader:
        return BodyReader(io.BufferedReader(io.BytesIO(body)), headers, limit)

    def test_reads_a_declared_body_in_pieces(self):
        reader = self.reader(b"abcdefghij", {"Content-Length": "10"})
        assert reader.declared_size() == 10
        assert [reader.read(4), reader.read(4), reader.read(4), reader.read(4)] == [b"abcd", b"efgh", b"ij", b""]

    def test_a_missing_content_length_is_an_empty_body(self):
        reader = self.reader(b"ignored", {})
        assert reader.declared_size() == 0
        assert reader.read_all() == b""

    def test_refuses_a_declared_body_over_the_limit_before_reading(self):
        reader = self.reader(b"x" * 20, {"Content-Length": "20"}, limit=10)
        with pytest.raises(BadRequest) as e:
            reader.read()
        assert e.value.status == 413

    def test_refuses_a_chunked_body_once_past_the_limit(self):
        reader = self.reader(b"a\r\n0123456789\r\na\r\n0123456789\r\n0\r\n\r\n", {"Transfer-Encoding": "chunked"}, limit=15)
        assert reader.declared_size() is None
        assert reader.read(10) == b"0123456789"
        with pytest.raises(BadRequest) as e:
            reader.read(10)
        assert e.value.status == 413

    def test_drain_reads_the_rest_up_to_a_bound(self):
        reader = self.reader(b"x" * 30, {"Content-Length": "30"}, limit=10)
        assert reader.drain() is True
        reader = self.reader(b"x" * 40, {"Content-Length": "40"}, limit=10)
        assert reader.drain() is True, "the body ends at the bound"
        stream = io.BufferedReader(io.BytesIO(b"x" * 100))
        reader = BodyReader(stream, {"Content-Length": "100"}, 10)
        assert reader.drain() is False
        assert len(stream.read()) == 60, "only the bound was read"
        reader = self.reader(b"5\r\nabcde\r\n0\r\n\r\n", {"Transfer-Encoding": "chunked"}, limit=2)
        assert reader.drain() is True
        reader = self.reader(b"zz\r\n", {"Transfer-Encoding": "chunked"}, limit=2)
        assert reader.drain() is False

    def test_drain_gives_up_on_a_body_whose_framing_was_invalid(self):
        reader = self.reader(b"2\r\n{}XX\r\n0\r\n\r\n", {"Transfer-Encoding": "chunked"})
        with pytest.raises(BadRequest):
            reader.read()
        assert reader.drain() is False


class TestGrowingFile:
    def test_allocates_ahead_in_steps_and_cuts_to_size_at_the_end(self, tmp_path):
        fd = os.open(tmp_path / "f", os.O_RDWR | os.O_CREAT)
        try:
            out = GrowingFile(fd)
            out.write(b"a" * 10)
            assert os.fstat(fd).st_size == 10 + main.UPLOAD_GROW_STEP
            out.write(b"b" * 5)
            assert os.fstat(fd).st_size == 10 + main.UPLOAD_GROW_STEP
            out.finish()
            assert os.fstat(fd).st_size == 15
        finally:
            os.close(fd)
        assert (tmp_path / "f").read_bytes() == b"a" * 10 + b"b" * 5

    def test_a_known_size_bounds_the_allocation(self, tmp_path):
        fd = os.open(tmp_path / "f", os.O_RDWR | os.O_CREAT)
        try:
            out = GrowingFile(fd, expected_size=100)
            out.write(b"a" * 10)
            assert os.fstat(fd).st_size == 100
            out.write(b"a" * 90)
            assert os.fstat(fd).st_size == 100
            out.finish()
            assert os.fstat(fd).st_size == 100
        finally:
            os.close(fd)

    def test_doubles_what_has_arrived_towards_a_known_size(self, tmp_path):
        fd = os.open(tmp_path / "f", os.O_RDWR | os.O_CREAT)
        try:
            size = 3 * main.UPLOAD_GROW_STEP
            out = GrowingFile(fd, expected_size=size)
            out.write(b"a" * main.UPLOAD_GROW_STEP)
            assert os.fstat(fd).st_size == 2 * main.UPLOAD_GROW_STEP
            out.write(b"a" * (main.UPLOAD_GROW_STEP + 1))
            assert os.fstat(fd).st_size == size
        finally:
            os.close(fd)


class TestUpload:
    def test_writes_the_file_and_skips_other_fields(self, server, base_dir):
        body = multipart([("note", None, b"ignored"), ("file", "dir/hello.txt", b"hello --b0undary\r\nworld\r\n")])
        status, result = upload(server, body)
        assert status == 200
        assert result == {"message": "File 'dir/hello.txt' uploaded successfully."}
        assert (base_dir / "dir" / "hello.txt").read_bytes() == b"hello --b0undary\r\nworld\r\n"
        assert not (base_dir / UPLOADS_IN_PROGRESS).exists() or not os.listdir(base_dir / UPLOADS_IN_PROGRESS)

    def test_the_last_file_part_wins(self, server, base_dir):
        body = multipart([("file", "a.txt", b"first"), ("file", "b.txt", b"second")])
        assert upload(server, body)[0] == 200
        assert not (base_dir / "a.txt").exists()
        assert (base_dir / "b.txt").read_bytes() == b"second"

    def test_a_large_file_in_small_pieces_arrives_whole(self, server, base_dir):
        content = bytes(range(256)) * 4096  # 1 MiB
        status, _ = upload(server, multipart([("file", "big.bin", content)]))
        assert status == 200
        assert (base_dir / "big.bin").read_bytes() == content

    def test_a_chunked_upload(self, server, base_dir):
        body = multipart([("file", "c.txt", b"chunked content")])
        data = (
            b"POST /upload HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n"
            b"Content-Type: multipart/form-data; boundary=b0undary\r\n\r\n"
        )
        for start in range(0, len(body), 7):
            piece = body[start:start + 7]
            data += f"{len(piece):x}\r\n".encode() + piece + b"\r\n"
        response, _ = raw(server, data + b"0\r\n\r\n")
        assert response.status == 200
        assert (base_dir / "c.txt").read_bytes() == b"chunked content"

    def test_an_rfc_2231_boundary(self, server, base_dir):
        body = multipart([("file", "r.txt", b"rfc 2231")])
        status, _ = upload(server, body, "multipart/form-data; boundary*=utf-8''b0undary")
        assert status == 200
        assert (base_dir / "r.txt").read_bytes() == b"rfc 2231"

    def test_a_non_ascii_filename(self, server, base_dir):
        # Clients send a filename as raw UTF-8.
        status, result = upload(server, multipart([("file", "café.txt", b"x")]))
        assert status == 200
        assert (base_dir / "café.txt").read_bytes() == b"x"
        # Bytes that are not UTF-8 are replaced, not refused.
        body = (b'--b0undary\r\nContent-Disposition: form-data; name="file"; filename="caf\xe9.txt"\r\n\r\ny\r\n'
                b"--b0undary--\r\n")
        status, result = upload(server, body)
        assert status == 200
        assert (base_dir / "caf\ufffd.txt").read_bytes() == b"y"

    def test_delimiter_padding_and_a_preamble(self, server, base_dir):
        # Whitespace may pad a delimiter line, and the preamble is not a
        # part even where it mentions the boundary.
        body = (b"a preamble that says --b0undary mid-line\r\n"
                b"--b0undary \t\r\n"
                b'Content-Disposition: form-data; name="file"; filename="p.txt"\r\n\r\npadded\r\n'
                b"--b0undary-- \r\nan epilogue\r\n")
        status, result = upload(server, body)
        assert status == 200, result
        assert (base_dir / "p.txt").read_bytes() == b"padded"

    def test_refuses_a_path_out_of_the_base_after_reading_the_body(self, server, base_dir):
        # The escape is seen in the part's headers, with the content still
        # to come; the answer waits for it.
        body = multipart([("file", "../escape.txt", b"x" * 300000)])
        status, result = upload(server, body)
        assert status == 403
        assert result == {"message": "Access denied"}
        assert not (base_dir.parent / "escape.txt").exists()

    @pytest.mark.parametrize("filename", ["", ".", UPLOADS_IN_PROGRESS + "/x", "adir"])
    def test_refuses_a_name_that_is_not_a_file(self, server, base_dir, filename):
        (base_dir / "adir").mkdir()
        status, result = upload(server, multipart([("file", filename, b"x")]))
        assert status in (400, 422), result
        assert not os.listdir(base_dir / UPLOADS_IN_PROGRESS) if (base_dir / UPLOADS_IN_PROGRESS).exists() else True

    def test_refuses_a_body_without_a_file_part(self, server):
        status, result = upload(server, multipart([("note", None, b"x")]))
        assert status == 422
        assert result == {"detail": "Expected a multipart 'file' field"}

    def test_refuses_a_body_that_is_not_multipart(self, server):
        response, data = request(server, "POST", "/upload", b"{}", {"Content-Type": "application/json"})
        assert response.status == 422

    def test_refuses_a_body_that_ends_early(self, server, base_dir):
        body = multipart([("file", "e.txt", b"content")])
        cut = body[: body.index(b"--b0undary--")]
        status, result = upload(server, cut)
        assert status == 400
        assert result == {"detail": "Multipart body ended early"}
        assert not (base_dir / "e.txt").exists()
        assert not os.listdir(base_dir / UPLOADS_IN_PROGRESS)

    def test_refuses_an_upload_over_the_limit(self, server, base_dir, monkeypatch):
        monkeypatch.setenv("SANDBOX_MAX_UPLOAD_BYTES", "1024")
        status, result = upload(server, multipart([("file", "l.txt", b"x" * 4000)]))
        assert status == 413
        assert result == {"detail": "Request body larger than 1024 bytes"}
        assert not (base_dir / "l.txt").exists()

    def test_refuses_oversized_part_headers(self, server):
        body = b"--b0undary\r\nX-Long: " + b"y" * (main.MAX_MULTIPART_HEADER + 10) + b"\r\n\r\nx\r\n--b0undary--\r\n"
        status, result = upload(server, body)
        assert status == 400
        assert result == {"detail": "Multipart headers too large"}


class TestFiles:
    def test_download_streams_the_file(self, server, base_dir):
        (base_dir / "d").mkdir()
        content = b"0123456789" * 100000
        (base_dir / "d" / "fü.bin").write_bytes(content)
        response, data = request(server, "GET", "/download/d%2Ff%C3%BC.bin")
        assert response.status == 200
        assert response.getheader("Content-Type") == "application/octet-stream"
        assert response.getheader("Content-Length") == str(len(content))
        assert response.getheader("Content-Disposition") == "attachment; filename*=utf-8''f%C3%BC.bin"
        assert data == content

    def test_download_of_what_is_not_a_file(self, server, base_dir):
        (base_dir / "adir").mkdir()
        (base_dir / UPLOADS_IN_PROGRESS).mkdir()
        (base_dir / UPLOADS_IN_PROGRESS / "tmp").write_bytes(b"x")
        for path in ("/download/missing", "/download/adir", f"/download/{UPLOADS_IN_PROGRESS}%2Ftmp"):
            assert request(server, "GET", path)[0].status == 404, path
        assert request(server, "GET", "/download/..%2Fout")[0].status == 403

    def test_list_hides_the_uploads_dir_and_survives_a_dangling_symlink(self, server, base_dir):
        (base_dir / "f.txt").write_bytes(b"abc")
        (base_dir / "sub").mkdir()
        (base_dir / UPLOADS_IN_PROGRESS).mkdir()
        os.symlink("/nowhere", base_dir / "dangling")
        response, data = request(server, "GET", "/list/")
        assert response.status == 200
        entries = {e["name"]: e for e in json.loads(data)}
        assert set(entries) == {"f.txt", "sub", "dangling"}
        assert entries["f.txt"]["size"] == 3 and entries["f.txt"]["type"] == "file"
        assert entries["sub"]["type"] == "directory"
        assert entries["dangling"]["type"] == "file"
        assert request(server, "GET", f"/list/{UPLOADS_IN_PROGRESS}")[0].status == 404
        assert request(server, "GET", "/list/f.txt")[0].status == 404

    def test_exists(self, server, base_dir):
        (base_dir / "here").write_bytes(b"")
        (base_dir / UPLOADS_IN_PROGRESS).mkdir()
        for path, expected in (("here", True), ("gone", False), (UPLOADS_IN_PROGRESS, False)):
            response, data = request(server, "GET", f"/exists/{path}")
            assert response.status == 200
            assert json.loads(data) == {"path": path, "exists": expected}
        assert request(server, "GET", "/exists/..%2Fx")[0].status == 403


class TestConnections:
    def test_one_connection_serves_several_requests(self, server):
        conn = connect(server)
        try:
            for i in range(3):
                conn.request("POST", "/execute", json.dumps({"command": f"echo {i}"}).encode(),
                             {"Content-Type": "application/json"})
                response = conn.getresponse()
                assert response.status == 200
                assert json.loads(response.read())["stdout"] == f"{i}\n"
                assert response.getheader("Connection") is None
            conn.request("GET", "/")
            assert conn.getresponse().read()
        finally:
            conn.close()

    def test_a_refused_body_that_was_read_keeps_the_connection(self, server):
        conn = connect(server)
        try:
            conn.request("POST", "/upload", multipart([("note", None, b"x")]),
                         {"Content-Type": "multipart/form-data; boundary=b0undary"})
            response = conn.getresponse()
            assert response.status == 422 and response.read()
            assert response.getheader("Connection") is None
            conn.request("GET", "/")
            assert conn.getresponse().status == 200
        finally:
            conn.close()

    def test_a_get_with_a_body_ends_the_connection(self, server):
        response, _ = request(server, "GET", "/", b"unexpected")
        assert response.status == 200
        assert response.getheader("Connection") == "close"

    def test_a_client_leaving_mid_download_is_one_log_line(self, server, base_dir, caplog, capsys):
        caplog.set_level(logging.INFO)
        # More than the two sockets can buffer, so the write is cut short.
        with open(base_dir / "big", "wb") as f:
            f.truncate(256 << 20)
        with socket.create_connection(("127.0.0.1", server.server_address[1])) as sock:
            sock.sendall(b"GET /download/big HTTP/1.1\r\nHost: x\r\n\r\n")
            sock.recv(1024)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, b"\x01\x00\x00\x00\x00\x00\x00\x00")
        for _ in range(50):
            if any("left mid-response" in r.getMessage() for r in caplog.records):
                break
            time.sleep(0.1)
        assert any("left mid-response" in r.getMessage() for r in caplog.records)
        assert "Traceback" not in capsys.readouterr().err
