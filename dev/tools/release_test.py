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

"""Unit tests for the drift-detection helpers in dev/tools/release."""

import importlib.util
import os
import tempfile
import textwrap
import unittest
from importlib.machinery import SourceFileLoader

# The release tool is an extensionless script, so load it via importlib rather
# than a normal import. An explicit SourceFileLoader is required because the
# filename has no `.py` extension for importlib to infer one. It is import-safe:
# execution is guarded by `if __name__ == "__main__"`.
_RELEASE_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "release")
_loader = SourceFileLoader("release", _RELEASE_PATH)
_spec = importlib.util.spec_from_loader("release", _loader)
release = importlib.util.module_from_spec(_spec)
_loader.exec_module(release)


def _write_kustomization(body):
    """Write kustomization YAML to a temp file and return its path."""
    tmp = tempfile.NamedTemporaryFile(
        mode="w", suffix=".yaml", delete=False
    )
    tmp.write(textwrap.dedent(body))
    tmp.close()
    return tmp.name


class KustomizationResourcePathsTest(unittest.TestCase):
    def setUp(self):
        self._paths = []

    def tearDown(self):
        for p in self._paths:
            os.unlink(p)

    def _parse(self, body):
        path = _write_kustomization(body)
        self._paths.append(path)
        return release._kustomization_resource_paths(path)

    def test_simple_list(self):
        got = self._parse(
            """
            resources:
              - controller.yaml
              - crds/agents.x-k8s.io_sandboxes.yaml
            """
        )
        self.assertEqual(got, {"controller.yaml", "crds/agents.x-k8s.io_sandboxes.yaml"})

    def test_ignores_comments_and_blanks(self):
        got = self._parse(
            """
            # a full-line comment
            resources:
              # a comment inside the block

              - controller.yaml
            """
        )
        self.assertEqual(got, {"controller.yaml"})

    def test_strips_inline_comment(self):
        # Regression: an inline comment must not become part of the filename.
        got = self._parse(
            """
            resources:
              - controller.yaml # core controller
              - extensions.yaml   #  extra spaces before hash
            """
        )
        self.assertEqual(got, {"controller.yaml", "extensions.yaml"})

    def test_stops_at_next_top_level_key(self):
        got = self._parse(
            """
            resources:
              - controller.yaml
            patches:
              - patch: |-
                  - op: add
            """
        )
        # The `- patch: ...` under `patches:` must not be collected.
        self.assertEqual(got, {"controller.yaml"})

    def test_empty_without_resources_block(self):
        got = self._parse(
            """
            apiVersion: kustomize.config.k8s.io/v1beta1
            kind: Kustomization
            """
        )
        self.assertEqual(got, set())


class CheckInstallManifestDriftTest(unittest.TestCase):
    def setUp(self):
        self._paths = []

    def tearDown(self):
        for p in self._paths:
            os.unlink(p)

    def _kustomization(self, *resources):
        body = "resources:\n" + "".join(f"  - {r}\n" for r in resources)
        path = _write_kustomization(body)
        self._paths.append(path)
        return path

    def test_in_sync_passes(self):
        kpath = self._kustomization("controller.yaml", "extensions.yaml")
        # No SystemExit expected.
        release.check_install_manifest_drift(
            ["k8s/controller.yaml", "k8s/extensions.yaml"],
            kustomization_path=kpath,
            k8s_dir="k8s",
        )

    def test_extensions_controller_is_excluded(self):
        # extensions.controller.yaml is globbed but intentionally NOT listed in
        # kustomization (the core controller.yaml is patched instead), so it must
        # not be reported as drift.
        kpath = self._kustomization("controller.yaml", "extensions.yaml")
        release.check_install_manifest_drift(
            [
                "k8s/controller.yaml",
                "k8s/extensions.controller.yaml",
                "k8s/extensions.yaml",
            ],
            kustomization_path=kpath,
            k8s_dir="k8s",
        )

    def test_missing_file_is_drift(self):
        kpath = self._kustomization("controller.yaml")
        with self.assertRaises(SystemExit):
            release.check_install_manifest_drift(
                ["k8s/controller.yaml", "k8s/new-thing.yaml"],
                kustomization_path=kpath,
                k8s_dir="k8s",
            )

    def test_extra_listed_file_is_drift(self):
        kpath = self._kustomization("controller.yaml", "ghost.yaml")
        with self.assertRaises(SystemExit):
            release.check_install_manifest_drift(
                ["k8s/controller.yaml"],
                kustomization_path=kpath,
                k8s_dir="k8s",
            )


if __name__ == "__main__":
    unittest.main()
