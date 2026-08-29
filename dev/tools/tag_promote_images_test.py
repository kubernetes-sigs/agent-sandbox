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

"""Unit tests for dev/tools/tag-promote-images."""

import importlib.util
import os
import sys
import tempfile
import textwrap
import unittest
from importlib.machinery import SourceFileLoader
import yaml

_TOOLS_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _TOOLS_DIR)
_TAG_PROMOTE_PATH = os.path.join(_TOOLS_DIR, "tag-promote-images")
_loader = SourceFileLoader("tag_promote_images", _TAG_PROMOTE_PATH)
_spec = importlib.util.spec_from_loader("tag_promote_images", _loader)
tag_promote = importlib.util.module_from_spec(_spec)
_loader.exec_module(tag_promote)


class TagPromoteImagesListTest(unittest.TestCase):
    """Tests that IMAGES_TO_PROMOTE contains the expected images."""

    def test_images_to_promote_contains_required_images(self):
        expected = [
            "agent-sandbox-controller",
            "chrome-sandbox",
            "python-runtime-sandbox",
            "sandbox-router-go",
        ]
        self.assertEqual(tag_promote.IMAGES_TO_PROMOTE, expected)


class UpdateImagesYamlTest(unittest.TestCase):
    """Tests updating images.yaml with new digests."""

    def setUp(self):
        self._temp_files = []

    def tearDown(self):
        for f in self._temp_files:
            if os.path.exists(f):
                os.unlink(f)

    def _write_temp_yaml(self, content):
        tmp = tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False)
        tmp.write(textwrap.dedent(content))
        tmp.close()
        self._temp_files.append(tmp.name)
        return tmp.name

    def test_update_images_yaml_inserts_digests(self):
        sample_yaml = """
        images:
          - name: agent-sandbox-controller
            dmap:
              "sha256:old1": ["v0.1.0"]
          - name: chrome-sandbox
            dmap:
              "sha256:old2": ["v0.1.0"]
          - name: python-runtime-sandbox
            dmap:
              "sha256:old3": ["v0.1.0"]
          - name: sandbox-router-go
            dmap:
              "sha256:old4": ["v0.1.0"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": "sha256:new1",
            "chrome-sandbox": "sha256:new2",
            "python-runtime-sandbox": "sha256:new3",
            "sandbox-router-go": "sha256:new4",
        }

        tag_promote.update_images_yaml(yaml_path, "v0.2.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        self.assertIn('"sha256:new1": ["v0.2.0"]', content)
        self.assertIn('"sha256:new2": ["v0.2.0"]', content)
        self.assertIn('"sha256:new3": ["v0.2.0"]', content)
        self.assertIn('"sha256:new4": ["v0.2.0"]', content)
        # Old entries should be preserved
        self.assertIn('"sha256:old1": ["v0.1.0"]', content)

    def test_update_images_yaml_replaces_duplicate_tag(self):
        sample_yaml = """
        images:
          - name: sandbox-router-go
            dmap:
              "sha256:old_v2": ["v0.2.0"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "sandbox-router-go": "sha256:new_v2",
        }

        tag_promote.update_images_yaml(yaml_path, "v0.2.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        self.assertIn('"sha256:new_v2": ["v0.2.0"]', content)
        self.assertNotIn('"sha256:old_v2": ["v0.2.0"]', content)

    def test_update_images_yaml_appends_missing_image_block(self):
        sample_yaml = """
        images:
          - name: agent-sandbox-controller
            dmap:
              "sha256:old1": ["v0.1.0"]
          - name: chrome-sandbox
            dmap:
              "sha256:old2": ["v0.1.0"]
          - name: python-runtime-sandbox
            dmap:
              "sha256:old3": ["v0.1.0"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": "sha256:new1",
            "chrome-sandbox": "sha256:new2",
            "python-runtime-sandbox": "sha256:new3",
            "sandbox-router-go": "sha256:new4",
        }

        tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        self.assertIn('"sha256:new1": ["v1.0.0"]', content)
        self.assertIn('"sha256:new2": ["v1.0.0"]', content)
        self.assertIn('"sha256:new3": ["v1.0.0"]', content)
        self.assertIn("- name: sandbox-router-go", content)
        self.assertIn('"sha256:new4": ["v1.0.0"]', content)

    def test_update_images_yaml_appends_missing_image_block_top_level(self):
        sample_yaml = """- name: agent-sandbox-controller
  dmap:
    "sha256:old1": ["v0.1.0"]
- name: chrome-sandbox
  dmap:
    "sha256:old2": ["v0.1.0"]
"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": "sha256:new1",
            "chrome-sandbox": "sha256:new2",
            "sandbox-router-go": "sha256:new4",
        }

        tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        self.assertIn("- name: sandbox-router-go\n  dmap:\n    \"sha256:new4\": [\"v1.0.0\"]", content)

    def test_update_images_yaml_fails_when_digest_is_none(self):
        sample_yaml = """- name: agent-sandbox-controller
  dmap:
    "sha256:old1": ["v0.1.0"]
"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": None,
        }

        with self.assertRaises(SystemExit) as cm:
            tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)
        self.assertEqual(cm.exception.code, 1)

    def test_update_images_yaml_appends_to_empty_wrapped_images_manifest(self):
        sample_yaml = """images:\n"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "sandbox-router-go": "sha256:new4",
        }

        tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        self.assertIn("images:\n  - name: sandbox-router-go\n    dmap:\n      \"sha256:new4\": [\"v1.0.0\"]", content)
        parsed = yaml.safe_load(content)
        self.assertEqual(
            parsed,
            {
                "images": [
                    {
                        "name": "sandbox-router-go",
                        "dmap": {
                            "sha256:new4": ["v1.0.0"],
                        },
                    }
                ]
            },
        )

    def test_update_images_yaml_fails_when_existing_block_missing_dmap(self):
        sample_yaml = """- name: sandbox-router-go
  some_other_field: val
"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "sandbox-router-go": "sha256:new4",
        }

        with self.assertRaises(SystemExit) as cm:
            tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)
        self.assertEqual(cm.exception.code, 1)

        # Ensure duplicate block was not appended
        with open(yaml_path, "r") as f:
            content = f.read()
        self.assertEqual(content.count("- name: sandbox-router-go"), 1)


if __name__ == "__main__":
    unittest.main()

