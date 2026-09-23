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
from unittest import mock
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

    def test_update_images_yaml_appends_to_flow_style_empty_images_manifest(self):
        sample_yaml = """images: []\n"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "sandbox-router-go": "sha256:new4",
        }

        tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

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

    def test_update_images_yaml_exact_name_matching_prevents_prefix_collision(self):
        sample_yaml = """- name: sandbox-router-go
  dmap:
    "sha256:old_go": ["v0.1.0"]
- name: sandbox-router
  dmap:
    "sha256:old_router": ["v0.1.0"]
"""
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "sandbox-router": "sha256:new_router",
            "sandbox-router-go": "sha256:new_go",
        }

        tag_promote.update_images_yaml(yaml_path, "v1.0.0", collected_digests)

        with open(yaml_path, "r") as f:
            content = f.read()

        parsed = yaml.safe_load(content)
        self.assertEqual(
            parsed,
            [
                {
                    "name": "sandbox-router-go",
                    "dmap": {
                        "sha256:new_go": ["v1.0.0"],
                        "sha256:old_go": ["v0.1.0"],
                    },
                },
                {
                    "name": "sandbox-router",
                    "dmap": {
                        "sha256:new_router": ["v1.0.0"],
                        "sha256:old_router": ["v0.1.0"],
                    },
                },
            ],
        )

    def test_parse_image_name(self):
        self.assertEqual(tag_promote.parse_image_name("- name: my-image"), "my-image")
        self.assertEqual(tag_promote.parse_image_name("  - name: my-image"), "my-image")
        self.assertEqual(tag_promote.parse_image_name("- name: 'my-image'"), "my-image")
        self.assertEqual(tag_promote.parse_image_name('- name: "my-image"'), "my-image")
        self.assertEqual(tag_promote.parse_image_name("- name: my-image # comment"), "my-image")
        self.assertIsNone(tag_promote.parse_image_name("- name:"))
        self.assertIsNone(tag_promote.parse_image_name("# - name: commented"))
        self.assertIsNone(tag_promote.parse_image_name("images: []"))


class UpdateImagesYamlChartTest(unittest.TestCase):
    """Tests the charts/agent-sandbox promotion path."""

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

    def test_chart_block_added_with_both_destination_tags(self):
        sample_yaml = """
        images:
          - name: agent-sandbox-controller
            dmap:
              "sha256:old1": ["v1.2.2"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": "sha256:new1",
            "charts/agent-sandbox": ("sha256:chart1", ["1.2.3"]),
        }

        tag_promote.update_images_yaml(yaml_path, "v1.2.3", collected_digests)

        with open(yaml_path) as f:
            parsed = yaml.safe_load(f)
        by_name = {b["name"]: b for b in parsed["images"]}

        # Existing container-image behavior is unaffected: new digest is
        # tagged with just the release tag, prior releases' history stays.
        self.assertEqual(
            by_name["agent-sandbox-controller"]["dmap"],
            {"sha256:new1": ["v1.2.3"], "sha256:old1": ["v1.2.2"]},
        )
        # The chart gets both the release tag and its bare semver.
        self.assertEqual(by_name["charts/agent-sandbox"]["dmap"], {"sha256:chart1": ["v1.2.3", "1.2.3"]})

    def test_chart_rerun_is_idempotent_and_does_not_duplicate(self):
        sample_yaml = """
        images:
          - name: charts/agent-sandbox
            dmap:
              "sha256:chart1": ["v1.2.3", "1.2.3"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {"charts/agent-sandbox": ("sha256:chart1", ["1.2.3"])}

        tag_promote.update_images_yaml(yaml_path, "v1.2.3", collected_digests)

        with open(yaml_path) as f:
            content = f.read()
        self.assertEqual(content.count("charts/agent-sandbox"), 1)
        self.assertEqual(content.count("sha256:chart1"), 1)
        parsed = yaml.safe_load(content)
        chart_block = next(b for b in parsed["images"] if b["name"] == "charts/agent-sandbox")
        self.assertEqual(chart_block["dmap"], {"sha256:chart1": ["v1.2.3", "1.2.3"]})

    def test_chart_rerun_with_changed_digest_replaces_both_old_tags(self):
        sample_yaml = """
        images:
          - name: charts/agent-sandbox
            dmap:
              "sha256:chart_old": ["v1.2.3", "1.2.3"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {"charts/agent-sandbox": ("sha256:chart_new", ["1.2.3"])}

        tag_promote.update_images_yaml(yaml_path, "v1.2.3", collected_digests)

        with open(yaml_path) as f:
            content = f.read()
        self.assertNotIn("sha256:chart_old", content)
        parsed = yaml.safe_load(content)
        chart_block = next(b for b in parsed["images"] if b["name"] == "charts/agent-sandbox")
        self.assertEqual(chart_block["dmap"], {"sha256:chart_new": ["v1.2.3", "1.2.3"]})

    def test_chart_appended_as_new_block_with_two_tags(self):
        sample_yaml = """
        images:
          - name: agent-sandbox-controller
            dmap:
              "sha256:old1": ["v1.2.2"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {"charts/agent-sandbox": ("sha256:chart1", ["1.2.3"])}

        tag_promote.update_images_yaml(yaml_path, "v1.2.3", collected_digests)

        with open(yaml_path) as f:
            parsed = yaml.safe_load(f)
        by_name = {b["name"]: b for b in parsed["images"]}
        self.assertEqual(by_name["charts/agent-sandbox"]["dmap"], {"sha256:chart1": ["v1.2.3", "1.2.3"]})

    def test_mixed_images_and_chart_promoted_in_one_run(self):
        """A full promotion run: existing image history for other tags is
        preserved while both the images and the chart get this run's tag(s)."""
        sample_yaml = """
        images:
          - name: agent-sandbox-controller
            dmap:
              "sha256:old1": ["v1.2.2"]
          - name: chrome-sandbox
            dmap:
              "sha256:old2": ["v1.2.2"]
          - name: python-runtime-sandbox
            dmap:
              "sha256:old3": ["v1.2.2"]
          - name: sandbox-router-go
            dmap:
              "sha256:old4": ["v1.2.2"]
          - name: charts/agent-sandbox
            dmap:
              "sha256:old_chart": ["v1.2.2", "1.2.2"]
        """
        yaml_path = self._write_temp_yaml(sample_yaml)
        collected_digests = {
            "agent-sandbox-controller": "sha256:new1",
            "chrome-sandbox": "sha256:new2",
            "python-runtime-sandbox": "sha256:new3",
            "sandbox-router-go": "sha256:new4",
            "charts/agent-sandbox": ("sha256:new_chart", ["1.2.3"]),
        }

        tag_promote.update_images_yaml(yaml_path, "v1.2.3", collected_digests)

        with open(yaml_path) as f:
            content = f.read()
        parsed = yaml.safe_load(content)
        by_name = {b["name"]: b for b in parsed["images"]}

        self.assertEqual(by_name["agent-sandbox-controller"]["dmap"]["sha256:new1"], ["v1.2.3"])
        self.assertEqual(by_name["charts/agent-sandbox"]["dmap"]["sha256:new_chart"], ["v1.2.3", "1.2.3"])
        # Prior releases' history for an unrelated tag is untouched.
        self.assertIn('"sha256:old1": ["v1.2.2"]', content)
        self.assertIn('"sha256:old_chart": ["v1.2.2", "1.2.2"]', content)


class GetChartDigestTest(unittest.TestCase):

    def test_returns_digest_on_first_successful_poll(self):
        with mock.patch.object(tag_promote, "run_command", return_value="sha256:abc") as run_cmd, \
             mock.patch.object(tag_promote.time, "sleep") as sleep:
            digest = tag_promote.get_chart_digest("1.2.3")

        self.assertEqual(digest, "sha256:abc")
        sleep.assert_not_called()
        called_args = run_cmd.call_args[0][0]
        self.assertIn(tag_promote.STAGING_CHART_REPO, called_args)
        self.assertIn(r"--filter=tags~^1\.2\.3$", called_args)

    def test_returns_none_after_timeout(self):
        with mock.patch.object(tag_promote, "run_command", return_value=None), \
             mock.patch.object(tag_promote.time, "sleep"):
            digest = tag_promote.get_chart_digest("1.2.3")
        self.assertIsNone(digest)


class IsStableReleaseTagTest(unittest.TestCase):
    """The chart-promotion gate in main() relies on this from shared.git_ops."""

    def test_stable_tags_are_accepted(self):
        for tag in ("v1.0.0", "v0.1.0", "v10.20.30"):
            self.assertTrue(tag_promote.is_stable_release_tag(tag), tag)

    def test_prerelease_and_malformed_tags_are_rejected(self):
        for tag in ("v1.0.0-rc1", "v1.0.0rc1", "v1.0.0.post1", "1.0.0", "v1.0"):
            self.assertFalse(tag_promote.is_stable_release_tag(tag), tag)


if __name__ == "__main__":
    unittest.main()

