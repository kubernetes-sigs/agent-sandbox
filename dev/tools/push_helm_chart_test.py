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

import importlib.util
import os
import sys
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
            ("0.3.10", "v0.3.10"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.3.10"),
        )

    def test_plain_release_tag(self):
        self.assertEqual(
            ("0.3.10", "v0.3.10"),
            push_helm_chart.resolve_versions("0.1.2", "v0.3.10"),
        )

    def test_prow_rc_release_tag(self):
        self.assertEqual(
            ("0.3.10-rc.1", "v0.3.10-rc.1"),
            push_helm_chart.resolve_versions("0.1.2", "v20261102-v0.3.10-rc.1"),
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
        )
        cmd = mock_call.call_args[0][0]
        self.assertEqual(
            ["helm", "package", "/repo/helm", "--destination", "/tmp/out",
             "--version", "0.3.10", "--app-version", "v0.3.10"],
            cmd[-9:],
        )

    @mock.patch.object(push_helm_chart.subprocess, "check_call")
    def test_push_chart_passes_charts_repo_without_chart_name(self, mock_call):
        push_helm_chart.push_chart(
            "/repo", "/tmp/out", self.PREFIX, "agent-sandbox", "0.3.10"
        )
        cmd = mock_call.call_args[0][0]
        self.assertEqual(
            ["helm", "push", "/tmp/out/agent-sandbox-0.3.10.tgz",
             f"oci://{self.PREFIX}/charts"],
            cmd[-4:],
        )


class TestParser(unittest.TestCase):

    def test_defaults_with_empty_environment(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            args = push_helm_chart.make_parser().parse_args([])
            self.assertIsNone(args.image_prefix)
            self.assertIsNone(args.git_tag)
            self.assertFalse(args.push)


if __name__ == "__main__":
    unittest.main()
