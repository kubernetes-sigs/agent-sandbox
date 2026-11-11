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

"""Unit tests for the sandboxd runtime path of the Python SDK.

These exercise the REST filesystem surface (via a mocked connector) plus
model parsing and config validation. They deliberately avoid the gRPC
command path, which requires generated stubs + the native grpcio extension.
"""

import datetime
import io
import unittest
from unittest.mock import MagicMock

from k8s_agent_sandbox import (
    SandboxdInClusterConnectionConfig as PublicInClusterConfig,
    SandboxNoServiceError,
    SandboxServiceUnavailableError,
)
from k8s_agent_sandbox.exceptions import SandboxRequestError
from k8s_agent_sandbox.files.filesystem import Filesystem
from k8s_agent_sandbox.models import (
    FileEntry,
    SandboxdInClusterConnectionConfig,
    SandboxdPodTunnelConnectionConfig,
    SandboxdRouterTunnelConnectionConfig,
)


class TestFileEntryParsing(unittest.TestCase):
    def test_from_legacy_float_timestamp(self):
        e = FileEntry.from_legacy(
            {"name": "a.txt", "size": 5, "type": "file", "mod_time": 1700000000.0}
        )
        self.assertEqual(e.name, "a.txt")
        self.assertEqual(e.size, 5)
        self.assertEqual(e.type, "file")
        self.assertEqual(e.modified.tzinfo, datetime.timezone.utc)
        self.assertIsNone(e.mode)

    def test_from_sandboxd_rfc3339(self):
        e = FileEntry.from_sandboxd(
            {
                "name": "b.txt",
                "size": 10,
                "type": "file",
                "modified_at": "2026-08-06T10:00:00Z",
                "mode": "0644",
            }
        )
        self.assertEqual(e.name, "b.txt")
        self.assertEqual(e.mode, "0644")
        self.assertEqual(
            e.modified,
            datetime.datetime(2026, 8, 6, 10, 0, 0, tzinfo=datetime.timezone.utc),
        )


class TestSandboxdConfig(unittest.TestCase):
    def test_router_config_defaults(self):
        from k8s_agent_sandbox import SandboxdRouterTunnelConnectionConfig as PublicRouterConfig

        self.assertIs(PublicRouterConfig, SandboxdRouterTunnelConnectionConfig)
        config = SandboxdRouterTunnelConnectionConfig()
        self.assertEqual((config.rest_port, config.grpc_port), (8080, 9090))
        self.assertEqual(config.router_namespace, "agent-sandbox-system")
        self.assertEqual(config.port_forward_ready_timeout, 30)
        self.assertIsNone(config.router_auth_token)
        self.assertFalse(config.pod_ip_routing)

    def test_router_config_validation(self):
        for values in (
            {"rest_port": 0}, {"grpc_port": 65536},
            {"rest_port": 9090}, {"port_forward_ready_timeout": 0},
            {"router_namespace": ""}, {"router_namespace": "Invalid"},
            {"router_namespace": "a" * 64}, {"router_namespace": "ns\n"},
            {"router_auth_token": ""}, {"router_auth_token": "has space"},
        ):
            with self.subTest(values=values), self.assertRaises(ValueError):
                SandboxdRouterTunnelConnectionConfig(**values)

    def test_defaults(self):
        cfg = SandboxdPodTunnelConnectionConfig()
        self.assertEqual(cfg.rest_port, 8080)
        self.assertEqual(cfg.grpc_port, 9090)

    def test_invalid_port_rejected(self):
        with self.assertRaises(ValueError):
            SandboxdPodTunnelConnectionConfig(rest_port=70000)

    def test_direct_in_cluster_requires_mode_and_has_two_ports(self):
        self.assertIs(PublicInClusterConfig, SandboxdInClusterConnectionConfig)
        self.assertTrue(issubclass(SandboxNoServiceError, RuntimeError))
        self.assertIs(SandboxServiceUnavailableError, SandboxNoServiceError)
        with self.assertRaises(ValueError):
            SandboxdInClusterConnectionConfig()
        config = SandboxdInClusterConnectionConfig(mode="in-cluster-service")
        self.assertEqual((config.rest_port, config.grpc_port), (8080, 9090))

    def test_direct_in_cluster_maps_deprecated_modes(self):
        for old, new in (
            ("service-dns", "in-cluster-service"),
            ("pod-ip", "in-cluster-pod-ip"),
        ):
            with self.subTest(mode=old):
                with self.assertWarns(DeprecationWarning):
                    config = SandboxdInClusterConnectionConfig(mode=old)
                self.assertEqual(config.mode, new)

    def test_direct_in_cluster_rejects_invalid_mode_and_ports(self):
        for values in (
            {"mode": "auto"},
            {"mode": "in-cluster-pod-ip", "rest_port": 0},
            {"mode": "in-cluster-pod-ip", "grpc_port": 65536},
            {"mode": "in-cluster-pod-ip", "rest_port": 9090, "grpc_port": 9090},
        ):
            with self.subTest(values=values), self.assertRaises(ValueError):
                SandboxdInClusterConnectionConfig(**values)


class TestSandboxdFilesystem(unittest.TestCase):
    def setUp(self):
        self._connector = MagicMock()
        self._connector.is_sandboxd.return_value = True
        self._fs = Filesystem(self._connector, MagicMock(), trace_service_name="test")

    def _last_call(self):
        return self._connector.send_request.call_args

    def test_write_puts_raw_body(self):
        self._fs.write("dir/script.py", b"print(1)")
        args, kwargs = self._last_call()
        self.assertEqual(args[0], "PUT")
        self.assertEqual(args[1], "v1/files/dir/script.py")
        self.assertEqual(kwargs["data"], b"print(1)")
        self.assertEqual(kwargs["headers"]["Content-Type"], "application/octet-stream")

    def test_write_streams_raw_body_once(self):
        source = io.BytesIO(b"skip-streamed payload")
        source.seek(5)

        self._fs.write("dir/data.bin", source)

        args, kwargs = self._last_call()
        self.assertEqual(args[0], "PUT")
        self.assertEqual(b"".join(kwargs["data"]), b"streamed payload")
        self.assertTrue(kwargs["_disable_retries"])
        self.assertFalse(source.closed)

    def test_write_append_and_create_only_are_not_retried(self):
        self._fs.write("log.txt", b"x", append=True)
        args, kwargs = self._last_call()
        self.assertEqual(args[:2], ("PUT", "v1/files/log.txt?append=true"))
        self.assertTrue(kwargs["_disable_retries"])
        self.assertNotIn("If-None-Match", kwargs["headers"])

        self._fs.write("new.txt", b"x", create_only=True)
        args, kwargs = self._last_call()
        self.assertEqual(args[:2], ("PUT", "v1/files/new.txt"))
        self.assertEqual(kwargs["headers"]["If-None-Match"], "*")
        self.assertTrue(kwargs["_disable_retries"])

    def test_plain_write_stays_retryable(self):
        self._fs.write("a.txt", b"x")
        self.assertFalse(self._last_call().kwargs["_disable_retries"])

    def test_read_gets_files_endpoint(self):
        resp = MagicMock()
        resp.content = b"hello"
        self._connector.send_request.return_value = resp
        data = self._fs.read("notes/hello.txt")
        self.assertEqual(data, b"hello")
        args, _ = self._last_call()
        self.assertEqual(args[0], "GET")
        self.assertEqual(args[1], "v1/files/notes/hello.txt")

    def test_list_unwraps_directory_listing(self):
        resp = MagicMock()
        resp.json.return_value = {
            "path": "/notes",
            "entries": [
                {"name": "a.txt", "size": 5, "type": "file",
                 "modified_at": "2026-08-06T10:00:00Z", "mode": "0644"},
                {"name": "sub", "size": 0, "type": "directory",
                 "modified_at": "2026-08-06T11:00:00Z", "mode": "0755"},
            ],
        }
        self._connector.send_request.return_value = resp
        entries = self._fs.list("notes")
        self.assertEqual(len(entries), 2)
        self.assertEqual(entries[0].name, "a.txt")
        self.assertEqual(entries[0].mode, "0644")
        self.assertEqual(entries[1].type, "directory")

    def test_exists_true_on_head_ok(self):
        resp = MagicMock()
        resp.status_code = 200
        self._connector.send_request.return_value = resp
        self.assertTrue(self._fs.exists("present.txt"))
        args, kwargs = self._last_call()
        self.assertEqual(args[0], "HEAD")
        self.assertEqual(args[1], "v1/files/present.txt")
        # 404 must be passed as an allowed status so the connector does not
        # raise (and tear down the connection) on a missing file.
        self.assertIn(404, kwargs["allowed_statuses"])

    def test_exists_false_on_head_404(self):
        resp = MagicMock()
        resp.status_code = 404
        self._connector.send_request.return_value = resp
        self.assertFalse(self._fs.exists("absent.txt"))

    def test_exists_reraises_non_404(self):
        # A non-allowed error status still propagates (connector raises).
        self._connector.send_request.side_effect = SandboxRequestError(
            "boom", status_code=500)
        with self.assertRaises(SandboxRequestError):
            self._fs.exists("x.txt")

    def test_delete_sends_recursive_query(self):
        self._fs.delete("dir", recursive=True)
        args, _ = self._last_call()
        self.assertEqual(args[0], "DELETE")
        self.assertEqual(args[1], "v1/files/dir?recursive=true")

    def test_delete_non_recursive(self):
        self._fs.delete("f.txt")
        args, _ = self._last_call()
        self.assertEqual(args[1], "v1/files/f.txt")

    def test_delete_rejects_empty_path(self):
        with self.assertRaisesRegex(ValueError, "path must not be empty"):
            self._fs.delete("")
        self._connector.send_request.assert_not_called()

    def test_mkdir_sends_directory_type(self):
        self._fs.mkdir("a/b")
        args, _ = self._last_call()
        self.assertEqual(args[:2], ("PUT", "v1/files/a/b?type=directory"))

    def test_mkdir_parents(self):
        self._fs.mkdir("a/b", parents=True)
        args, _ = self._last_call()
        self.assertEqual(
            args[:2], ("PUT", "v1/files/a/b?type=directory&parents=true"))

    def test_move_and_copy_send_destination(self):
        for op in ("move", "copy"):
            with self.subTest(op=op):
                getattr(self._fs, op)("src dir/a.txt", "dst/b.txt")
                args, _ = self._last_call()
                self.assertEqual(args[0], "POST")
                self.assertEqual(
                    args[1],
                    f"v1/files/src%20dir/a.txt:{op}?destination=dst%2Fb.txt")

    def test_move_overwrite(self):
        self._fs.move("a", "b", overwrite=True)
        args, _ = self._last_call()
        self.assertEqual(
            args[1], "v1/files/a:move?destination=b&overwrite=true")

    def test_mkdir_move_copy_reject_empty_arguments(self):
        for call in (
            lambda: self._fs.mkdir(""),
            lambda: self._fs.move("", "b"),
            lambda: self._fs.copy("a", ""),
        ):
            with self.subTest(), self.assertRaisesRegex(ValueError, "must not be empty"):
                call()
        self._connector.send_request.assert_not_called()

    def test_extract_sends_tar_body(self):
        self._fs.extract("src dir", b"tar-bytes")
        args, kwargs = self._last_call()
        self.assertEqual(args[:2], ("PUT", "v1/files/src%20dir?extract=tar"))
        self.assertEqual(kwargs["data"], b"tar-bytes")
        self.assertFalse(kwargs["_disable_retries"])

    def test_extract_streams_file_object_without_retries(self):
        self._fs.extract("src", io.BytesIO(b"tar-bytes"))
        _, kwargs = self._last_call()
        self.assertEqual(b"".join(kwargs["data"]), b"tar-bytes")
        self.assertTrue(kwargs["_disable_retries"])

    def test_extract_accepts_the_sandbox_root(self):
        for path in (".", "/"):
            with self.subTest(path=path):
                self._fs.extract(path, b"tar-bytes")
                args, _ = self._last_call()
                self.assertEqual(args[:2], ("PUT", "v1/files/?extract=tar"))

    def test_other_ops_reject_the_sandbox_root(self):
        for call in (
            lambda: self._fs.mkdir("/"),
            lambda: self._fs.move(".", "b"),
            lambda: self._fs.copy("/", "b"),
        ):
            with self.subTest(), self.assertRaisesRegex(ValueError, "sandbox root"):
                call()
        self._connector.send_request.assert_not_called()

    def test_extract_rejects_bad_arguments(self):
        for call, error in (
            (lambda: self._fs.extract("", b"x"), ValueError),
            (lambda: self._fs.extract("src", io.StringIO("x")), TypeError),
        ):
            with self.subTest(), self.assertRaises(error):
                call()
        self._connector.send_request.assert_not_called()


class TestLegacyFileOpsUnsupported(unittest.TestCase):
    def test_sandboxd_only_ops_raise_on_legacy(self):
        connector = MagicMock()
        connector.is_sandboxd.return_value = False
        fs = Filesystem(connector, MagicMock(), trace_service_name="test")
        for call in (
            lambda: fs.delete("f.txt"),
            lambda: fs.mkdir("d"),
            lambda: fs.move("a", "b"),
            lambda: fs.copy("a", "b"),
            lambda: fs.extract("d", b"x"),
            lambda: fs.write("a", b"x", append=True),
            lambda: fs.write("a", b"x", create_only=True),
        ):
            with self.subTest(), self.assertRaises(NotImplementedError):
                call()
        connector.send_request.assert_not_called()


class TestProcessStubs(unittest.TestCase):
    def test_stub_import_and_construct(self):
        try:
            import grpc  # noqa: F401
            from k8s_agent_sandbox.commands._process_stubs import (
                process_pb2,
                process_pb2_grpc,
            )
        except ImportError:
            self.skipTest("grpc extra not installed; sandboxd gRPC path is opt-in")
        req = process_pb2.ExecuteRequest(
            config=process_pb2.ProcessConfig(
                command=["/bin/sh", "-c", "echo hello"],
                cwd="/workspace",
                env_vars={"FOO": "BAR"},
            )
        )
        self.assertEqual(list(req.config.command), ["/bin/sh", "-c", "echo hello"])
        self.assertEqual(req.config.cwd, "/workspace")
        self.assertEqual(req.config.env_vars["FOO"], "BAR")
        self.assertTrue(hasattr(process_pb2_grpc, "ProcessServiceStub"))
        self.assertTrue(hasattr(process_pb2_grpc, "ProcessServiceServicer"))


if __name__ == "__main__":
    unittest.main()
