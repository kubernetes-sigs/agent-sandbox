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

"""Unit tests for the sandboxd gRPC status -> exception mapping."""

import unittest

from k8s_agent_sandbox._grpc_errors import classify_rpc_error
from k8s_agent_sandbox.exceptions import (
    SandboxConnectionError,
    SandboxdPermissionError,
    SandboxdProcessNotFoundError,
    SandboxdRpcError,
    SandboxTimeoutError,
)


def _require_grpc():
    """Report whether the optional grpc extra is installed."""
    try:
        import grpc  # noqa: F401
    except ImportError:
        return None
    return grpc


class TestClassifyRpcError(unittest.TestCase):
    """The shared gRPC status -> exception mapping.

    Asserted against the real grpc.StatusCode enum rather than a
    SimpleNamespace fake, so a reclassification regression cannot pass by
    agreeing with a stand-in.
    """

    def setUp(self):
        if _require_grpc() is None:
            self.skipTest(
                "grpc extra not installed; sandboxd gRPC path is opt-in")

    @staticmethod
    def _rpc_error(code, details="boom"):
        import grpc

        err = grpc.RpcError()
        err.code = lambda: code
        err.details = lambda: details
        return err

    def test_status_codes_map_to_their_classes(self):
        import grpc

        cases = (
            (grpc.StatusCode.DEADLINE_EXCEEDED, SandboxTimeoutError, None),
            (grpc.StatusCode.UNAVAILABLE, SandboxConnectionError, "unavailable"),
            (grpc.StatusCode.CANCELLED, SandboxConnectionError, "socket"),
            (grpc.StatusCode.PERMISSION_DENIED, SandboxdPermissionError,
             "permission_denied"),
            (grpc.StatusCode.NOT_FOUND, SandboxdRpcError, "not_found"),
            (grpc.StatusCode.RESOURCE_EXHAUSTED, SandboxdRpcError,
             "resource_exhausted"),
            (grpc.StatusCode.INVALID_ARGUMENT, SandboxdRpcError,
             "invalid_argument"),
            (grpc.StatusCode.UNAUTHENTICATED, SandboxdRpcError,
             "unauthenticated"),
            (grpc.StatusCode.FAILED_PRECONDITION, SandboxdRpcError,
             "failed_precondition"),
            (grpc.StatusCode.INTERNAL, SandboxdRpcError, "internal"),
            (grpc.StatusCode.UNIMPLEMENTED, SandboxdRpcError, "unimplemented"),
            # ABORTED stays in the generic bucket: unlike Connect-ES, grpcio
            # reports transport resets as UNAVAILABLE, so it carries no
            # transport meaning here.
            (grpc.StatusCode.ABORTED, SandboxdRpcError, "aborted"),
        )
        for code, expected, discriminant in cases:
            with self.subTest(code=code):
                err = classify_rpc_error(
                    self._rpc_error(code), operation="Execute")
                self.assertIsInstance(err, expected)
                if expected is SandboxConnectionError:
                    self.assertEqual(err.kind, discriminant)
                elif discriminant is not None:
                    self.assertEqual(err.code, discriminant)

    def test_not_found_names_the_process_only_when_one_is_given(self):
        import grpc

        err = classify_rpc_error(
            self._rpc_error(grpc.StatusCode.NOT_FOUND),
            operation="SendSignal",
            process_id=77,
        )
        self.assertIsInstance(err, SandboxdProcessNotFoundError)
        self.assertIn("process 77 not found", str(err))

        generic = classify_rpc_error(
            self._rpc_error(grpc.StatusCode.NOT_FOUND), operation="Execute")
        self.assertIsInstance(generic, SandboxdRpcError)
        self.assertNotIsInstance(generic, SandboxdProcessNotFoundError)

    def test_unrecognized_status_reports_unknown(self):
        err = classify_rpc_error(self._rpc_error(None), operation="Execute")
        self.assertIsInstance(err, SandboxdRpcError)
        self.assertEqual(err.code, "unknown")

    def test_server_text_reaches_detail_and_not_the_message(self):
        """The message is fixed; the server's text is opt-in via detail.

        A message travels into logs, tracebacks and span status, so the
        unbounded, server-controlled string stays off it.
        """
        import grpc

        err = classify_rpc_error(
            self._rpc_error(grpc.StatusCode.INTERNAL, "x" * 5000),
            operation="Execute",
        )
        self.assertIsNotNone(err.detail)
        self.assertTrue(err.detail.endswith("... [truncated]"))
        self.assertLess(len(err.detail), 5000)
        self.assertNotIn("xxx", str(err))
        self.assertEqual(
            str(err), f"sandboxd Execute failed ({grpc.StatusCode.INTERNAL})")

    def test_empty_details_leave_detail_unset(self):
        import grpc

        err = classify_rpc_error(
            self._rpc_error(grpc.StatusCode.INTERNAL, ""), operation="Execute")
        self.assertIsNone(err.detail)

    def test_a_non_grpc_error_is_handed_back_unchanged(self):
        """Only a gRPC failure gets a gRPC classification.

        Wrapping anything else would report a local error -- a bug in this
        SDK, say -- as a status sandboxd returned.
        """
        oops = AttributeError("'NoneType' object has no attribute 'Execute'")
        self.assertIs(classify_rpc_error(oops, operation="Execute"), oops)

    def test_typed_errors_stay_catchable_as_the_builtins_they_replace(self):
        # Backward compat: callers written against the previous behaviour
        # catch TimeoutError / PermissionError / LookupError.
        import grpc

        self.assertIsInstance(
            classify_rpc_error(
                self._rpc_error(grpc.StatusCode.DEADLINE_EXCEEDED),
                operation="Execute"),
            TimeoutError,
        )
        self.assertIsInstance(
            classify_rpc_error(
                self._rpc_error(grpc.StatusCode.PERMISSION_DENIED),
                operation="Execute"),
            PermissionError,
        )
        self.assertIsInstance(
            classify_rpc_error(
                self._rpc_error(grpc.StatusCode.NOT_FOUND),
                operation="SendSignal", process_id=1),
            LookupError,
        )
        # And every one of them is still a RuntimeError, as SandboxError was
        # before this taxonomy existed.
        self.assertIsInstance(
            classify_rpc_error(
                self._rpc_error(grpc.StatusCode.INTERNAL), operation="Execute"),
            RuntimeError,
        )


if __name__ == "__main__":
    unittest.main()
