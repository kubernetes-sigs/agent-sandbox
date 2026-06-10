#!/usr/bin/env python3

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

import os
import sys
import time
import unittest

# Make the test importable regardless of how it is invoked (python -m unittest,
# pytest from any cwd, etc.).
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from headers import _match_path_parts, is_path_excluded


class MatchPathPartsTest(unittest.TestCase):
    """Tests for the DP-based path/pattern matcher."""

    def match(self, path, pattern):
        return _match_path_parts(path.split('/'), pattern.split('/'))

    def test_exact_match(self):
        self.assertTrue(self.match("a/b/c", "a/b/c"))
        self.assertFalse(self.match("a/b/c", "a/b/d"))

    def test_length_mismatch_without_wildcard(self):
        self.assertFalse(self.match("a/b", "a/b/c"))
        self.assertFalse(self.match("a/b/c", "a/b"))

    def test_single_star_globs_one_component(self):
        self.assertTrue(self.match("a/foo.py", "a/*.py"))
        self.assertFalse(self.match("a/b/foo.py", "a/*.py"))

    def test_double_star_matches_zero_components(self):
        self.assertTrue(self.match("a/b", "a/**/b"))

    def test_double_star_matches_many_components(self):
        self.assertTrue(self.match("a/x/y/z/b", "a/**/b"))

    def test_leading_double_star(self):
        self.assertTrue(self.match("x/y/vendor", "**/vendor"))
        self.assertTrue(self.match("vendor", "**/vendor"))

    def test_trailing_slash_matches_directory(self):
        # Regression: a trailing slash produces an empty trailing component,
        # which must behave as an epsilon transition (match zero parts).
        self.assertTrue(self.match("foo", "foo/"))
        self.assertTrue(self.match("a/foo", "a/foo/"))

    def test_internal_empty_component_from_double_slash(self):
        self.assertTrue(self.match("a/b", "a//b"))

    def test_multiple_double_stars(self):
        self.assertTrue(self.match("a/x/y/b/z/c", "a/**/b/**/c"))
        self.assertTrue(self.match("a/b/c", "**/**/c"))

    def test_redos_input_completes_quickly(self):
        # The whole point of the DP rewrite: a deeply nested path against a
        # pattern with many '**' components used to be exponential. Assert it
        # now resolves quickly (O(N*M)) rather than hanging.
        path = "/".join(["a"] * 60)
        pattern = "/".join(["**"] * 30 + ["nope"])
        start = time.monotonic()
        result = _match_path_parts(path.split('/'), pattern.split('/'))
        elapsed = time.monotonic() - start
        self.assertFalse(result)
        self.assertLess(elapsed, 1.0)


class IsPathExcludedTest(unittest.TestCase):
    """Tests for the public is_path_excluded entry point."""

    def test_no_slash_matches_any_component(self):
        self.assertTrue(is_path_excluded("a/b/node_modules/x.js", ["node_modules"]))
        self.assertTrue(is_path_excluded("a/b/foo.py", ["*.py"]))
        self.assertFalse(is_path_excluded("a/b/foo.py", ["*.go"]))

    def test_slash_pattern_matches_from_root(self):
        self.assertTrue(is_path_excluded("vendor/lib/x.go", ["vendor/**"]))
        self.assertFalse(is_path_excluded("a/vendor/x.go", ["vendor/**"]))

    def test_trailing_slash_pattern(self):
        self.assertTrue(is_path_excluded("build", ["build/"]))

    def test_no_patterns(self):
        self.assertFalse(is_path_excluded("a/b/c", []))


if __name__ == "__main__":
    unittest.main()
