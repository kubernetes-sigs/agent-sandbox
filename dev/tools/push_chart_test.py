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

"""Unit tests for dev/tools/push-chart."""

import argparse
import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

import yaml

_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_PUSH_CHART_PATH = os.path.join(_SCRIPT_DIR, "push-chart")
_loader = SourceFileLoader("push_chart", _PUSH_CHART_PATH)
_spec = importlib.util.spec_from_loader("push_chart", _loader)
push_chart = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(push_chart)
sys.modules["push_chart"] = push_chart

REAL_CHART_DIR = os.path.join(os.path.dirname(_SCRIPT_DIR), "..", "helm")
REAL_CHART_DIR = os.path.normpath(REAL_CHART_DIR)


def _fake_git_tags(tags):
    """A subprocess.run stand-in returning the given tag names for `git tag --points-at`."""

    def _run(cmd, **kwargs):
        assert cmd[:3] == ["git", "tag", "--points-at"]
        return subprocess.CompletedProcess(cmd, 0, stdout="\n".join(tags), stderr="")

    return _run


class FindHeadReleaseTagTest(unittest.TestCase):

    def test_stable_tag_on_head_is_detected(self):
        with mock.patch.object(push_chart.subprocess, "run", _fake_git_tags(["v1.2.3"])):
            self.assertEqual(push_chart.find_head_release_tag("/repo"), "v1.2.3")

    def test_no_tag_on_head_returns_none(self):
        with mock.patch.object(push_chart.subprocess, "run", _fake_git_tags([])):
            self.assertIsNone(push_chart.find_head_release_tag("/repo"))

    def test_prerelease_tag_on_head_is_ignored(self):
        with mock.patch.object(push_chart.subprocess, "run", _fake_git_tags(["v1.2.3-rc1"])):
            self.assertIsNone(push_chart.find_head_release_tag("/repo"))

    def test_multiple_stable_tags_on_head_are_ambiguous(self):
        with mock.patch.object(push_chart.subprocess, "run", _fake_git_tags(["v1.2.3", "v1.2.4"])):
            self.assertIsNone(push_chart.find_head_release_tag("/repo"))


class PrepareReleaseChartCopyTest(unittest.TestCase):
    """Exercises the real helm/ chart directory so schema drift (e.g. a
    renamed `image` key) is caught by a test failure rather than silently
    shipping a chart with an unset image.tag."""

    def test_packaged_copy_gets_pinned_image_tag(self):
        with tempfile.TemporaryDirectory() as tmp:
            chart_copy = push_chart.prepare_release_chart_copy(REAL_CHART_DIR, tmp, image_tag="v1.2.3")
            with open(os.path.join(chart_copy, "values.yaml")) as f:
                values = yaml.safe_load(f)
            self.assertEqual(values["image"]["tag"], "v1.2.3")
            # image.repository is untouched.
            self.assertEqual(
                values["image"]["repository"],
                "registry.k8s.io/agent-sandbox/agent-sandbox-controller",
            )

    def test_source_values_yaml_is_not_modified(self):
        source_values_path = os.path.join(REAL_CHART_DIR, "values.yaml")
        with open(source_values_path, "rb") as f:
            before = f.read()

        with tempfile.TemporaryDirectory() as tmp:
            push_chart.prepare_release_chart_copy(REAL_CHART_DIR, tmp, image_tag="v1.2.3")

        with open(source_values_path, "rb") as f:
            after = f.read()
        self.assertEqual(before, after)


class SetImageTagTest(unittest.TestCase):
    """Tests the stdlib-only image.tag rewriter (no PyYAML in production)."""

    def _write_values(self, content):
        tmp = tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False)
        tmp.write(content)
        tmp.close()
        self.addCleanup(os.unlink, tmp.name)
        return tmp.name

    def test_replaces_image_tag(self):
        values_path = self._write_values(
            "image:\n"
            "  repository: example.com/foo\n"
            '  tag: ""\n'
            "  pullPolicy: IfNotPresent\n"
        )
        push_chart.set_image_tag(values_path, "v1.2.3")
        with open(values_path) as f:
            content = f.read()
        self.assertIn('  tag: "v1.2.3"\n', content)
        self.assertIn("repository: example.com/foo", content)

    def test_preserves_nearby_comment(self):
        values_path = self._write_values(
            "image:\n"
            "  repository: example.com/foo\n"
            '  tag: ""  # required, eg. v0.3.10\n'
            "  pullPolicy: IfNotPresent\n"
        )
        push_chart.set_image_tag(values_path, "v1.2.3")
        with open(values_path) as f:
            content = f.read()
        self.assertIn('  tag: "v1.2.3"  # required, eg. v0.3.10\n', content)

    def test_unrelated_tag_key_elsewhere_is_untouched(self):
        values_path = self._write_values(
            "image:\n"
            "  repository: example.com/foo\n"
            '  tag: ""\n'
            "\n"
            "someOtherSection:\n"
            "  tag: keep-me\n"
        )
        push_chart.set_image_tag(values_path, "v1.2.3")
        with open(values_path) as f:
            content = f.read()
        self.assertIn('  tag: "v1.2.3"\n', content)
        self.assertIn("  tag: keep-me\n", content)

    def test_fails_when_image_tag_is_missing(self):
        values_path = self._write_values(
            "image:\n"
            "  repository: example.com/foo\n"
            "  pullPolicy: IfNotPresent\n"
        )
        with self.assertRaises(ValueError):
            push_chart.set_image_tag(values_path, "v1.2.3")

    def test_fails_when_image_mapping_is_missing(self):
        values_path = self._write_values("replicaCount: 1\n")
        with self.assertRaises(ValueError):
            push_chart.set_image_tag(values_path, "v1.2.3")

    def test_fails_when_structure_is_ambiguous(self):
        values_path = self._write_values(
            "image:\n"
            "  tag: foo\n"
            "  tag: bar\n"
        )
        with self.assertRaises(ValueError):
            push_chart.set_image_tag(values_path, "v1.2.3")

    def test_real_values_yaml_round_trips_through_yaml_parser(self):
        """Sanity check against the real chart: the rewritten file must still
        parse as valid YAML with only image.tag changed."""
        with tempfile.TemporaryDirectory() as tmp:
            chart_copy = push_chart.prepare_release_chart_copy(REAL_CHART_DIR, tmp, image_tag="v1.2.3")
            with open(os.path.join(chart_copy, "values.yaml")) as f:
                values = yaml.safe_load(f)
        self.assertEqual(values["image"]["tag"], "v1.2.3")


class PackageAndPushArgsTest(unittest.TestCase):
    """Verifies the exact helm command lines, without requiring a real helm binary."""

    def test_package_chart_command_line(self):
        with mock.patch.object(push_chart.subprocess, "run") as run:
            result = push_chart.package_chart(
                "/tmp/chart-copy", "1.2.3", "v1.2.3", "/tmp/out", "/repo/bin/helm",
            )
        run.assert_called_once_with(
            [
                "/repo/bin/helm", "package",
                "--version", "1.2.3",
                "--app-version", "v1.2.3",
                "/tmp/chart-copy",
                "-d", "/tmp/out",
            ],
            check=True,
        )
        self.assertEqual(result, "/tmp/out/agent-sandbox-1.2.3.tgz")

    def test_push_package_command_line(self):
        with mock.patch.object(push_chart.subprocess, "run") as run:
            push_chart.push_package(
                "/tmp/out/agent-sandbox-1.2.3.tgz",
                "us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/charts",
                "/repo/bin/helm",
            )
        run.assert_called_once_with(
            [
                "/repo/bin/helm", "push",
                "/tmp/out/agent-sandbox-1.2.3.tgz",
                "oci://us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/charts",
            ],
            check=True,
        )


def _make_args(**overrides):
    base = argparse.Namespace(
        repo_root="/repo",
        chart_dir=REAL_CHART_DIR,
        helm_bin=sys.executable,  # any real, executable file; never actually invoked in these tests
        staging_repo="us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/charts",
        dest_dir=None,
        tag=None,
        dry_run=False,
    )
    for k, v in overrides.items():
        setattr(base, k, v)
    return base


class MainTest(unittest.TestCase):

    def test_non_release_checkout_does_not_package_or_push(self):
        with mock.patch.object(push_chart, "find_head_release_tag", return_value=None), \
             mock.patch.object(push_chart, "package_chart") as package, \
             mock.patch.object(push_chart, "push_package") as push:
            code = push_chart.main(_make_args())
        self.assertEqual(code, 0)
        package.assert_not_called()
        push.assert_not_called()

    def test_invalid_explicit_tag_does_not_package_or_push(self):
        with mock.patch.object(push_chart, "package_chart") as package, \
             mock.patch.object(push_chart, "push_package") as push:
            code = push_chart.main(_make_args(tag="v1.2.3-rc1"))
        self.assertEqual(code, 0)
        package.assert_not_called()
        push.assert_not_called()

    def test_stable_tag_packages_and_pushes_with_correct_versions(self):
        with tempfile.TemporaryDirectory() as dest_dir:
            with mock.patch.object(push_chart, "package_chart",
                                    return_value=os.path.join(dest_dir, "agent-sandbox-1.2.3.tgz")) as package, \
                 mock.patch.object(push_chart, "push_package") as push:
                code = push_chart.main(_make_args(tag="v1.2.3", dest_dir=dest_dir))

        self.assertEqual(code, 0)
        package.assert_called_once()
        _, chart_version, app_version, called_dest_dir, helm_bin = package.call_args[0]
        self.assertEqual(chart_version, "1.2.3")
        self.assertEqual(app_version, "v1.2.3")
        self.assertEqual(called_dest_dir, dest_dir)
        push.assert_called_once()

    def test_dry_run_packages_but_does_not_push(self):
        with tempfile.TemporaryDirectory() as dest_dir:
            with mock.patch.object(push_chart, "package_chart",
                                    return_value=os.path.join(dest_dir, "agent-sandbox-1.2.3.tgz")) as package, \
                 mock.patch.object(push_chart, "push_package") as push:
                code = push_chart.main(_make_args(tag="v1.2.3", dest_dir=dest_dir, dry_run=True))

        self.assertEqual(code, 0)
        package.assert_called_once()
        push.assert_not_called()

    def test_missing_helm_binary_is_an_error(self):
        with mock.patch.object(push_chart, "package_chart") as package, \
             mock.patch.object(push_chart, "push_package") as push:
            code = push_chart.main(_make_args(tag="v1.2.3", helm_bin="/nonexistent/helm"))
        self.assertEqual(code, 1)
        package.assert_not_called()
        push.assert_not_called()


class MakeParserTest(unittest.TestCase):

    def test_defaults(self):
        parser = push_chart.make_parser()
        args = parser.parse_args([])
        self.assertEqual(args.staging_repo, push_chart.STAGING_CHART_REPO)
        self.assertFalse(args.dry_run)
        self.assertIsNone(args.tag)


if __name__ == "__main__":
    unittest.main()
