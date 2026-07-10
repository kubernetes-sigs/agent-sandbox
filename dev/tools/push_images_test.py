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

"""Unit tests for the buildx invocation in dev/tools/push-images."""

import argparse
import importlib.util
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader

# The push-images tool is an extensionless script, so load it via importlib
# rather than a normal import (same pattern as release_test.py). The tools dir
# must be on sys.path first so its `from shared import utils` import resolves.
_TOOLS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _TOOLS_DIR)
_PUSH_IMAGES_PATH = os.path.join(_TOOLS_DIR, "push-images")
_loader = SourceFileLoader("push_images", _PUSH_IMAGES_PATH)
_spec = importlib.util.spec_from_loader("push_images", _loader)
push_images = importlib.util.module_from_spec(_spec)
_loader.exec_module(push_images)


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


if __name__ == "__main__":
    unittest.main()
