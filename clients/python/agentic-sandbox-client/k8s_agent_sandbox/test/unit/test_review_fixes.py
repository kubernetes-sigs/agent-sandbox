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

"""Regression tests for PR #310 review fixes (C4, J1, J3)."""

import asyncio
import unittest
from unittest.mock import MagicMock, patch

from k8s_agent_sandbox.files.async_filesystem import AsyncFilesystem
from k8s_agent_sandbox.files.filesystem import Filesystem
from k8s_agent_sandbox.sandbox import Sandbox


class TestFilesystemSafeUploadPath(unittest.TestCase):
    """C4: SDK must sanitize multipart filenames so the runtime cannot be
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


class TestSandboxTerminateIdempotent(unittest.TestCase):
    """J3: `Sandbox.terminate()` must be idempotent — a second call must not
    issue a redundant DELETE that would return 404."""

    @patch('k8s_agent_sandbox.sandbox.Filesystem')
    @patch('k8s_agent_sandbox.sandbox.CommandExecutor')
    @patch('k8s_agent_sandbox.sandbox.create_tracer_manager')
    @patch('k8s_agent_sandbox.sandbox.SandboxConnector')
    def _build_sandbox(self, mock_connector, mock_tracer, mock_cmd, mock_files):
        mock_tracer.return_value = (MagicMock(), MagicMock())
        from k8s_agent_sandbox.models import (
            SandboxLocalTunnelConnectionConfig, SandboxTracerConfig,
        )
        k8s_helper = MagicMock()
        return Sandbox(
            claim_name="my-claim",
            sandbox_id="my-claim",
            namespace="demo",
            connection_config=SandboxLocalTunnelConnectionConfig(),
            tracer_config=SandboxTracerConfig(),
            k8s_helper=k8s_helper,
        ), k8s_helper

    def test_second_terminate_does_not_redelete(self):
        sandbox, helper = self._build_sandbox()

        sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.call_count, 1)
        self.assertIsNone(sandbox.claim_name)

        # Second call must be a no-op.
        sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.call_count, 1)

    def test_failed_terminate_preserves_claim_name_for_retry(self):
        """When delete_sandbox_claim raises, claim_name must NOT be cleared —
        otherwise a transient 5xx / network blip would hide the error and
        the caller would have no handle to retry or clean up manually."""
        sandbox, helper = self._build_sandbox()

        helper.delete_sandbox_claim.side_effect = RuntimeError("transient 500")

        with self.assertRaisesRegex(RuntimeError, "transient 500"):
            sandbox.terminate()

        # claim_name must be preserved so the caller can retry.
        self.assertEqual(sandbox.claim_name, "my-claim")
        self.assertEqual(helper.delete_sandbox_claim.call_count, 1)

        # Retry succeeds and clears the handle.
        helper.delete_sandbox_claim.side_effect = None
        sandbox.terminate()
        self.assertEqual(helper.delete_sandbox_claim.call_count, 2)
        self.assertIsNone(sandbox.claim_name)


class TestSandboxClientTemplateVerification(unittest.TestCase):
    """J1: `get_sandbox(template_name=...)` must refuse to reconnect to a claim
    whose sandboxTemplateRef doesn't match the requested template."""

    def _build_client(self):
        from k8s_agent_sandbox.sandbox_client import SandboxClient
        with patch('k8s_agent_sandbox.sandbox_client.K8sHelper') as mock_helper_cls:
            client = SandboxClient()
            client.k8s_helper = mock_helper_cls.return_value
            return client

    def test_mismatched_template_raises_value_error(self):
        client = self._build_client()
        client.k8s_helper.get_sandbox_claim.return_value = {
            "spec": {"sandboxTemplateRef": {"name": "python-secure"}},
        }

        with self.assertRaisesRegex(ValueError, "references template 'python-secure'"):
            client.get_sandbox(
                "claim-1",
                namespace="demo",
                template_name="other-template",
            )

    def test_matching_template_does_not_short_circuit_reconnect(self):
        client = self._build_client()
        client.k8s_helper.get_sandbox_claim.return_value = {
            "spec": {"sandboxTemplateRef": {"name": "python-secure"}},
        }
        client.k8s_helper.resolve_sandbox_name.return_value = "sandbox-1"
        client.k8s_helper.get_sandbox.return_value = {"metadata": {"name": "sandbox-1"}}

        with patch.object(client, 'sandbox_class') as sandbox_cls:
            client.get_sandbox(
                "claim-1",
                namespace="demo",
                template_name="python-secure",
            )
            sandbox_cls.assert_called_once()

    def test_missing_claim_raises_not_found(self):
        from k8s_agent_sandbox.exceptions import SandboxNotFoundError
        client = self._build_client()
        client.k8s_helper.get_sandbox_claim.return_value = None

        with self.assertRaises(SandboxNotFoundError):
            client.get_sandbox(
                "claim-1",
                namespace="demo",
                template_name="python-secure",
            )


if __name__ == '__main__':
    unittest.main()
