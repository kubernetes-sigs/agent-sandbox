#!/usr/bin/env python3

# Copyright 2025 The Kubernetes Authors.
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

"""Unit tests for push-images — container engine dispatching and build context."""

import argparse
import importlib.util
import io
import os
import subprocess
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest.mock import patch, call, MagicMock

# Set up the path so 'from shared import utils' resolves correctly, then
# load the extensionless push-images script via importlib (same pattern as
# dev/tools/release_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_PUSH_IMAGES_PATH = os.path.join(_SCRIPT_DIR, "push-images")
_loader = SourceFileLoader("push_images", _PUSH_IMAGES_PATH)
_spec = importlib.util.spec_from_loader("push_images", _loader)
push_images = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(push_images)
# Register in sys.modules so @patch("push_images.xxx") resolves correctly.
sys.modules["push_images"] = push_images


def _make_args(**overrides):
    """Build a Namespace with the defaults expected by push_images helpers."""
    base = argparse.Namespace(
        image_prefix="kind.local/",
        extra_image_tags=[],
        kind_cluster_name=None,
        container_engine="docker",
        image_tag="test-tag",
    )
    for k, v in overrides.items():
        setattr(base, k, v)
    return base


# ---------------------------------------------------------------------------
# Buildx Dockerfile path resolution
# ---------------------------------------------------------------------------
class BuildxDockerfileArgTest(unittest.TestCase):
    """The -f argument must resolve relative to the build context (cwd).

    The sandbox-router-go image overrides the build context to the repo root
    while its Dockerfile stays at sandbox-router/Dockerfile; passing only the
    basename made buildx silently build the repo-root (controller) Dockerfile
    instead (issue #1123).
    """

    def _captured_build_cmd(self, srcdir, dockerfile_path):
        captured = {}

        def fake_run(cmd, **kwargs):
            captured["cmd"] = cmd
            captured["cwd"] = kwargs.get("cwd")

        orig_run = push_images.subprocess.run
        orig_version_args = push_images._get_version_build_args
        orig_image_name = push_images.utils.get_full_image_name
        push_images.subprocess.run = fake_run
        push_images._get_version_build_args = lambda: []
        push_images.utils.get_full_image_name = (
            lambda args, service_name, tag: f"example.local/{service_name}:{tag}"
        )
        try:
            args = argparse.Namespace(
                docker_build_output_type="docker",
                extra_image_tags=[],
            )
            push_images.build_and_push_image_with_docker_buildx(
                args, "svc", srcdir, dockerfile_path, "testtag"
            )
        finally:
            push_images.subprocess.run = orig_run
            push_images._get_version_build_args = orig_version_args
            push_images.utils.get_full_image_name = orig_image_name

        return captured

    def _dockerfile_arg(self, cmd):
        return cmd[cmd.index("-f") + 1]

    def test_per_directory_dockerfile_uses_basename(self):
        captured = self._captured_build_cmd(
            srcdir=os.path.join(".", "frobber"),
            dockerfile_path=os.path.join(".", "frobber", "Dockerfile"),
        )
        self.assertEqual(self._dockerfile_arg(captured["cmd"]), "Dockerfile")
        self.assertEqual(captured["cwd"], os.path.join(".", "frobber"))

    def test_context_override_keeps_dockerfile_path(self):
        # Mirrors the sandbox-router-go override in main(): context is the
        # repo root, the Dockerfile is not at the context root.
        captured = self._captured_build_cmd(
            srcdir=".",
            dockerfile_path=os.path.join(".", "sandbox-router", "Dockerfile"),
        )
        self.assertEqual(
            self._dockerfile_arg(captured["cmd"]),
            os.path.join("sandbox-router", "Dockerfile"),
        )
        self.assertEqual(captured["cwd"], ".")


# ---------------------------------------------------------------------------
# Container engine dispatching — podman vs docker
# ---------------------------------------------------------------------------
class CreateBuildxBuilderTest(unittest.TestCase):
    """create_buildx_builder_if_not_exists should skip buildx for podman."""

    @patch("push_images.subprocess")
    def test_skipped_for_podman(self, mock_subprocess):
        push_images.create_buildx_builder_if_not_exists("podman")
        mock_subprocess.run.assert_not_called()

    @patch("push_images.subprocess")
    def test_runs_for_docker(self, mock_subprocess):
        # docker buildx inspect fails → creates builder
        mock_subprocess.CalledProcessError = subprocess.CalledProcessError
        mock_subprocess.run.side_effect = [
            subprocess.CalledProcessError(1, "inspect"),  # inspect fails
            MagicMock(),  # create
            MagicMock(),  # use
        ]
        push_images.create_buildx_builder_if_not_exists("docker")
        self.assertEqual(mock_subprocess.run.call_count, 3)
        calls = mock_subprocess.run.call_args_list
        self.assertIn("'buildx', 'inspect'", str(calls[0]))
        self.assertIn("'buildx', 'create'", str(calls[1]))
        self.assertIn("'buildx', 'use'", str(calls[2]))


class BuildAndPushImageTest(unittest.TestCase):
    """build_and_push_image dispatches to the correct backend."""

    @patch("push_images.build_image_with_podman")
    @patch("push_images.build_and_push_image_with_docker_buildx")
    @patch("push_images.load_kind_image")
    def test_dispatches_to_podman_when_set(
        self, mock_load, mock_docker, mock_podman
    ):
        args = _make_args(container_engine="podman", kind_cluster_name="test-cluster")
        push_images.build_and_push_image(args, "svc", ".", "Dockerfile", "t1")
        mock_podman.assert_called_once()
        mock_docker.assert_not_called()
        mock_load.assert_called_once()

    @patch("push_images.build_image_with_podman")
    @patch("push_images.build_and_push_image_with_docker_buildx")
    @patch("push_images.load_kind_image")
    def test_dispatches_to_docker_by_default(
        self, mock_load, mock_docker, mock_podman
    ):
        args = _make_args(kind_cluster_name="test-cluster")
        push_images.build_and_push_image(args, "svc", ".", "Dockerfile", "t1")
        mock_docker.assert_called_once()
        mock_podman.assert_not_called()
        mock_load.assert_called_once()

    @patch("push_images.build_image_with_podman")
    @patch("push_images.build_and_push_image_with_docker_buildx")
    @patch("push_images.load_kind_image")
    def test_skips_kind_load_when_no_cluster(
        self, mock_load, mock_docker, mock_podman
    ):
        args = _make_args()
        push_images.build_and_push_image(args, "svc", ".", "Dockerfile", "t1")
        mock_load.assert_not_called()


class BuildImageWithPodmanTest(unittest.TestCase):
    """build_image_with_podman constructs the correct podman command."""

    @patch("push_images.subprocess.run")
    @patch("push_images._get_version_build_args", return_value=["--build-arg=X=1"])
    def test_podman_build_command(self, mock_version, mock_run):
        args = _make_args()
        push_images.build_image_with_podman(args, "my-service", "/ctx", "/ctx/Dockerfile", "t1")
        mock_run.assert_called_once()
        cmd = mock_run.call_args[0][0]
        self.assertEqual(cmd[0], "podman")
        self.assertIn("build", cmd)
        self.assertIn("-t", cmd)
        self.assertIn("kind.local/my-service:t1", cmd)
        self.assertIn("-f", cmd)
        self.assertIn("Dockerfile", cmd)
        self.assertIn(".", cmd)  # build context
        self.assertIn("--build-arg=X=1", cmd)
        self.assertEqual(mock_run.call_args[1]["cwd"], "/ctx")

    @patch("push_images.subprocess.run")
    @patch("push_images._get_version_build_args", return_value=[])
    def test_podman_extra_tags(self, mock_version, mock_run):
        args = _make_args(extra_image_tags=["v1", "latest"])
        push_images.build_image_with_podman(args, "my-service", ".", "Dockerfile", "t1")
        cmd = mock_run.call_args[0][0]
        # Count -t flags: one for primary tag + two extra = 3
        t_flags = [i for i, x in enumerate(cmd) if x == "-t"]
        self.assertEqual(len(t_flags), 3)
        self.assertIn("kind.local/my-service:t1", cmd)
        self.assertIn("kind.local/my-service:v1", cmd)
        self.assertIn("kind.local/my-service:latest", cmd)


class LoadKindImageTest(unittest.TestCase):
    """load_kind_image uses the right loading strategy per engine."""

    @patch("push_images.os.unlink")
    @patch("push_images.subprocess.run")
    @patch("push_images.tempfile.NamedTemporaryFile")
    def test_podman_uses_image_archive(self, mock_tempfile, mock_run, mock_unlink):
        # Simulate a temp file path
        mock_tmp = MagicMock()
        mock_tmp.name = "/tmp/podman-img.tar"
        mock_tempfile.return_value.__enter__.return_value = mock_tmp

        args = _make_args(container_engine="podman", kind_cluster_name="my-cluster")
        push_images.load_kind_image(args, "my-cluster", "svc", ".", "t1")

        # Two subprocess.run calls: podman save + kind load image-archive
        self.assertEqual(mock_run.call_count, 2)
        save_call = mock_run.call_args_list[0]
        load_call = mock_run.call_args_list[1]

        # Verify podman save --output <tmp> <image>
        save_cmd = save_call[0][0]
        self.assertEqual(save_cmd[0], "podman")
        self.assertIn("save", save_cmd)
        self.assertIn("--output", save_cmd)
        self.assertEqual(save_cmd[save_cmd.index("--output") + 1], "/tmp/podman-img.tar")
        self.assertIn("kind.local/svc:t1", save_cmd)

        # Verify kind load image-archive --name=my-cluster <tmp>
        load_cmd = load_call[0][0]
        self.assertEqual(load_cmd[0], "kind")
        self.assertIn("load", load_cmd)
        self.assertIn("image-archive", load_cmd)
        self.assertIn("--name=my-cluster", load_cmd)
        self.assertIn("/tmp/podman-img.tar", load_cmd)

        # Temp file was cleaned up
        mock_unlink.assert_called_once_with("/tmp/podman-img.tar")

    @patch("push_images.subprocess.run")
    def test_docker_uses_load_docker_image(self, mock_run):
        args = _make_args(kind_cluster_name="my-cluster")
        push_images.load_kind_image(args, "my-cluster", "svc", ".", "t1")
        mock_run.assert_called_once()
        cmd = mock_run.call_args[0][0]
        self.assertEqual(cmd[0], "kind")
        self.assertIn("load", cmd)
        self.assertIn("docker-image", cmd)
        self.assertIn("--name=my-cluster", cmd)
        self.assertIn("kind.local/svc:t1", cmd)


class CLITest(unittest.TestCase):
    """CLI argument parsing for --container-engine."""

    def setUp(self):
        self._old_env = os.environ.copy()

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self._old_env)

    def _make_parser(self):
        """Exercises the real parser from push-images so CLI tests don't drift."""
        return push_images.make_parser()

    def test_defaults_to_docker(self):
        os.environ.pop("CONTAINER_ENGINE", None)
        args = self._make_parser().parse_args([])
        self.assertEqual(args.container_engine, "docker")

    def test_defaults_from_env_var(self):
        os.environ["CONTAINER_ENGINE"] = "podman"
        args = self._make_parser().parse_args([])
        self.assertEqual(args.container_engine, "podman")

    def test_cli_overrides_env(self):
        os.environ["CONTAINER_ENGINE"] = "podman"
        args = self._make_parser().parse_args(["--container-engine", "docker"])
        self.assertEqual(args.container_engine, "docker")

    def test_rejects_invalid_value(self):
        os.environ.pop("CONTAINER_ENGINE", None)
        p = self._make_parser()
        # argparse prints its error to stderr before calling sys.exit; suppress it.
        with unittest.mock.patch("sys.stderr"):
            with self.assertRaises(SystemExit):
                p.parse_args(["--container-engine", "containerd"])

    def test_podman_requires_kind_cluster(self):
        """main() exits with an error when podman is used without --kind-cluster-name."""
        args = _make_args(container_engine="podman", kind_cluster_name=None)
        with self.assertRaises(SystemExit) as ctx:
            push_images.main(args)
        self.assertIn("kind-cluster-name", str(ctx.exception))


if __name__ == "__main__":
    unittest.main()
