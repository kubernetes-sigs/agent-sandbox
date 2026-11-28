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

"""Unit tests for push-helm-chart."""

import hashlib
import importlib.util
import io
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_loader = SourceFileLoader(
    "push_helm_chart", os.path.join(_SCRIPT_DIR, "push-helm-chart")
)
_spec = importlib.util.spec_from_loader("push_helm_chart", _loader)
push_helm_chart = importlib.util.module_from_spec(_spec)
_loader.exec_module(push_helm_chart)


class TestChartInfo(unittest.TestCase):

    def test_parses_name_and_version(self):
        contents = "apiVersion: v2\nname: agent-sandbox\nversion: 0.1.2\n"
        self.assertEqual(("agent-sandbox", "0.1.2"),
                         push_helm_chart.chart_info(contents))

    def test_parses_quoted_version(self):
        contents = "name: agent-sandbox\nversion: '0.1.2'\n"
        self.assertEqual(("agent-sandbox", "0.1.2"),
                         push_helm_chart.chart_info(contents))

    def test_requires_name(self):
        with self.assertRaises(ValueError):
            push_helm_chart.chart_info("version: 0.1.2\n")

    def test_requires_version(self):
        with self.assertRaises(ValueError):
            push_helm_chart.chart_info("name: agent-sandbox\n")


class TestResolveVersions(unittest.TestCase):

    def test_defaults_to_chart_yaml_version_without_git_tag(self):
        self.assertEqual(
            ("0.1.2", None),
            push_helm_chart.resolve_versions("0.1.2", None),
        )

    def test_prow_release_tag(self):
        self.assertEqual(
            ("0.3.10", "v20261102-v0.3.10"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.3.10"),
        )

    def test_plain_release_tag(self):
        self.assertEqual(
            ("0.3.10", "v0.3.10"),
            push_helm_chart.resolve_versions("0.1.2", "v0.3.10"),
        )

    def test_prow_rc_release_tag(self):
        self.assertEqual(
            ("0.3.10-rc.1", "v20261102-v0.3.10-rc.1"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.3.10-rc.1"),
        )

    def test_rc_without_hyphen_normalizes_to_semver(self):
        self.assertEqual(
            ("0.5.0-rc5", "v0.5.0rc5"),
            push_helm_chart.resolve_versions("0.1.2", "v0.5.0rc5"),
        )

    def test_dot_rc_normalizes_to_semver(self):
        self.assertEqual(
            ("0.4.4-rc1", "v0.4.4.rc1"),
            push_helm_chart.resolve_versions("0.1.2", "v0.4.4.rc1"),
        )

    def test_post_tag_normalizes_to_semver(self):
        self.assertEqual(
            ("0.3.10-post1", "v0.3.10.post1"),
            push_helm_chart.resolve_versions("0.1.2", "v0.3.10.post1"),
        )

    def test_prow_rc_without_hyphen_preserves_staging_app_version(self):
        self.assertEqual(
            ("0.5.0-rc5", "v20261102-v0.5.0rc5"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.5.0rc5"),
        )

    def test_prow_postsubmit_git_describe_tag(self):
        self.assertEqual(
            ("0.0.0-v20261102-v0.3.10-5-g2cc28d6", "v20261102-v0.3.10-5-g2cc28d6"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.3.10-5-g2cc28d6"),
        )

    def test_non_semver_git_tag_falls_back_to_prerelease(self):
        self.assertEqual(
            ("0.0.0-v20261102-2cc28d6", "v20261102-2cc28d6"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-2cc28d6"),
        )


class TestOciRef(unittest.TestCase):

    PREFIX = "us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox"

    def test_registry_host(self):
        self.assertEqual(
            "us-central1-docker.pkg.dev",
            push_helm_chart.registry_host(self.PREFIX),
        )

    def test_oci_charts_repo(self):
        self.assertEqual(
            f"oci://{self.PREFIX}/charts",
            push_helm_chart.oci_charts_repo(self.PREFIX),
        )

    @mock.patch.object(push_helm_chart.subprocess, "check_call")
    def test_package_chart_passes_version_and_app_version(self, mock_call):
        push_helm_chart.package_chart(
            "/repo", "/repo/helm", "/tmp/out",
            chart_version="0.3.10", app_version="v0.3.10",
            helm_bin="/bin/helm",
        )
        cmd = mock_call.call_args[0][0]
        self.assertEqual(
            ["/bin/helm", "package", "/repo/helm", "--destination", "/tmp/out",
             "--version", "0.3.10", "--app-version", "v0.3.10"],
            cmd,
        )

    @mock.patch.object(push_helm_chart.subprocess, "check_call")
    def test_push_chart_passes_charts_repo_without_chart_name(self, mock_call):
        push_helm_chart.push_chart(
            "/repo", "/tmp/out", self.PREFIX, "agent-sandbox", "0.3.10",
            helm_bin="/bin/helm",
        )
        cmd = mock_call.call_args[0][0]
        self.assertEqual(
            ["/bin/helm", "push", "/tmp/out/agent-sandbox-0.3.10.tgz",
             f"oci://{self.PREFIX}/charts"],
            cmd,
        )

    @mock.patch.object(push_helm_chart.time, "sleep")
    @mock.patch.object(push_helm_chart.subprocess, "check_call")
    def test_push_chart_retries_on_transient_failure(self, mock_call, mock_sleep):
        mock_call.side_effect = [
            subprocess.CalledProcessError(1, ["helm", "push"]),
            None,
        ]
        push_helm_chart.push_chart(
            "/repo", "/tmp/out", self.PREFIX, "agent-sandbox", "0.3.10",
            helm_bin="/bin/helm",
        )
        self.assertEqual(2, mock_call.call_count)
        mock_sleep.assert_called_once_with(2.0)


class TestEnsureHelm(unittest.TestCase):

    def _make_helm_tarball(self, os_name, arch, payload):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            info = tarfile.TarInfo(name=f"{os_name}-{arch}/helm")
            info.size = len(payload)
            info.mode = 0o755
            tf.addfile(info, io.BytesIO(payload))
        return buf.getvalue()

    def test_respects_helm_bin_env(self):
        with mock.patch.dict(os.environ, {"HELM_BIN": "/custom/helm"}):
            self.assertEqual("/custom/helm", push_helm_chart.ensure_helm("/repo"))

    def test_downloads_verifies_and_extracts_pinned_helm(self):
        payload = b"#!/bin/sh\necho helm\n"
        tarball = self._make_helm_tarball("linux", "amd64", payload)
        sha = hashlib.sha256(tarball).hexdigest()

        resp = mock.MagicMock()
        resp.__enter__.return_value = resp
        resp.read.return_value = tarball

        with tempfile.TemporaryDirectory() as repo_root, \
             mock.patch.dict(os.environ, {}, clear=True), \
             mock.patch.object(push_helm_chart, "host_platform", return_value=("linux", "amd64")), \
             mock.patch.dict(push_helm_chart.HELM_SHA256, {("linux", "amd64"): sha}), \
             mock.patch.object(push_helm_chart.urllib.request, "urlopen", return_value=resp) as mock_open:
            helm_path = push_helm_chart.ensure_helm(repo_root)
            self.assertTrue(os.path.isfile(helm_path))
            self.assertTrue(os.access(helm_path, os.X_OK))
            with open(helm_path, "rb") as f:
                self.assertEqual(payload, f.read())
            self.assertEqual(1, mock_open.call_count)

            # Second call uses cached binary without re-downloading.
            self.assertEqual(helm_path, push_helm_chart.ensure_helm(repo_root))
            self.assertEqual(1, mock_open.call_count)

    def test_rejects_checksum_mismatch(self):
        tarball = self._make_helm_tarball("linux", "amd64", b"bad")
        resp = mock.MagicMock()
        resp.__enter__.return_value = resp
        resp.read.return_value = tarball

        with tempfile.TemporaryDirectory() as repo_root, \
             mock.patch.dict(os.environ, {}, clear=True), \
             mock.patch.object(push_helm_chart, "host_platform", return_value=("linux", "amd64")), \
             mock.patch.dict(push_helm_chart.HELM_SHA256, {("linux", "amd64"): "0" * 64}), \
             mock.patch.object(push_helm_chart.urllib.request, "urlopen", return_value=resp):
            with self.assertRaises(RuntimeError):
                push_helm_chart.ensure_helm(repo_root)
            expected_path = os.path.join(
                repo_root, "dev", "tools", "tmp",
                f"helm-{push_helm_chart.HELM_VERSION}-linux-amd64", "helm",
            )
            self.assertFalse(os.path.exists(expected_path))


class TestParser(unittest.TestCase):

    def test_defaults_with_empty_environment(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            args = push_helm_chart.make_parser().parse_args([])
            self.assertIsNone(args.image_prefix)
            self.assertIsNone(args.git_tag)
            self.assertFalse(args.push)


if __name__ == "__main__":
    unittest.main()
