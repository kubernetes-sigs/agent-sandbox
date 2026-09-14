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

"""Unit tests for latest-published-tag — stable tag selection and pagination."""

import importlib.util
import json
import os
import sys
import unittest
from importlib.machinery import SourceFileLoader
from unittest import mock

# Load the extensionless latest-published-tag script via importlib (same
# pattern as dev/tools/push_images_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _SCRIPT_DIR)

_SCRIPT_PATH = os.path.join(_SCRIPT_DIR, "latest-published-tag")
_loader = SourceFileLoader("latest_published_tag", _SCRIPT_PATH)
_spec = importlib.util.spec_from_loader("latest_published_tag", _loader)
latest_published_tag = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(latest_published_tag)


class PickLatestStableTest(unittest.TestCase):
    def test_picks_newest_stable(self):
        tags = ["v0.4.5", "v0.5.3", "v0.1.1", "v0.5.5", "v0.5.0"]
        self.assertEqual(latest_published_tag.pick_latest_stable(tags), "v0.5.5")

    def test_ignores_prerelease_and_signature_tags(self):
        tags = [
            "v0.5.0rc1", "v0.5.0rc5", "v0.1.0-rc.2",
            "sha256-027a33ae4a4608ea26761978b8d14a0e1b03804b625f2d0a92b0cbddcb1536ef.sig",
            "sha256-4405ad751517c9f490fe7e4c6c419e8b565d98ee7583f0ad76c606ab18266b9b.att",
            "v0.4.6",
        ]
        self.assertEqual(latest_published_tag.pick_latest_stable(tags), "v0.4.6")

    def test_numeric_ordering_not_lexicographic(self):
        tags = ["v0.10.0", "v0.9.0", "v0.5.5"]
        self.assertEqual(latest_published_tag.pick_latest_stable(tags), "v0.10.0")

    def test_returns_none_when_no_stable_tags(self):
        self.assertIsNone(latest_published_tag.pick_latest_stable([]))
        self.assertIsNone(latest_published_tag.pick_latest_stable(["v0.5.0rc1", "v0.1.0-rc.2"]))


class _FakeResponse:
    """Minimal stand-in for urllib.request.urlopen responses."""

    def __init__(self, payload, headers=None):
        self._payload = payload
        self.headers = headers or {}

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        return False

    def read(self):
        return self._payload


def _page(tags, next_url=None):
    """Build a fake tags/list page, optionally advertising a next page."""
    headers = {"Link": f'<{next_url}>; rel="next"'} if next_url else {}
    return _FakeResponse(json.dumps({"tags": tags}).encode(), headers)


class FetchAllTagsTest(unittest.TestCase):
    def test_follows_next_links_across_pages(self):
        next_url = ("https://registry.k8s.io/v2/agent-sandbox/agent-sandbox-controller/"
                    "tags/list?n=2&last=v0.4.5")
        pages = [
            _page(["v0.4.5", "v0.5.0rc1"], next_url),
            _page(["v0.5.5"]),
        ]
        with mock.patch("urllib.request.urlopen", side_effect=pages) as urlopen:
            tags = latest_published_tag.fetch_all_tags()
        self.assertEqual(tags, ["v0.4.5", "v0.5.0rc1", "v0.5.5"])
        self.assertEqual(urlopen.call_count, 2)

    def test_picks_newest_stable_from_later_page(self):
        next_url = ("https://registry.k8s.io/v2/agent-sandbox/agent-sandbox-controller/"
                    "tags/list?n=2&last=v0.5.0")
        pages = [
            _page(["v0.4.5", "v0.5.0"], next_url),
            _page(["v0.6.0"]),
        ]
        with mock.patch("urllib.request.urlopen", side_effect=pages):
            tags = latest_published_tag.fetch_all_tags()
        self.assertEqual(latest_published_tag.pick_latest_stable(tags), "v0.6.0")

    def test_single_page_without_link_header(self):
        with mock.patch("urllib.request.urlopen", side_effect=[_page(["v0.5.5"])]) as urlopen:
            tags = latest_published_tag.fetch_all_tags()
        self.assertEqual(tags, ["v0.5.5"])
        self.assertEqual(urlopen.call_count, 1)

    def test_null_tags_are_ignored(self):
        # A valid OCI response may carry "tags": null; it must not crash.
        response = _FakeResponse(json.dumps({"tags": None}).encode())
        with mock.patch("urllib.request.urlopen", side_effect=[response]):
            tags = latest_published_tag.fetch_all_tags()
        self.assertEqual(tags, [])
        self.assertIsNone(latest_published_tag.pick_latest_stable(tags))


if __name__ == "__main__":
    unittest.main()

