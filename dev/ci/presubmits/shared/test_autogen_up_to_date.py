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

import importlib.util
import os
import subprocess
import sys
import tempfile
import unittest
from importlib.machinery import SourceFileLoader

# Load the extensionless test-autogen-up-to-date script via importlib (same
# pattern as dev/tools/verify_chart_version_test.py).
_SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "test-autogen-up-to-date")
_loader = SourceFileLoader("autogen_up_to_date", _SCRIPT)
_spec = importlib.util.spec_from_loader("autogen_up_to_date", _loader)
autogen = importlib.util.module_from_spec(_spec)
_loader.exec_module(autogen)


class TestAutogenUpToDate(unittest.TestCase):
    """Tests for how test-autogen-up-to-date detects changes."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.addCleanup(os.chdir, os.getcwd())
        os.chdir(tmp.name)
        subprocess.run(["git", "init", "-q"], check=True)

    def test_fails_on_new_untracked_file(self):
        with self.assertRaises(SystemExit):
            autogen.run_and_verify_no_changes([sys.executable, "-c", "open('new.yaml', 'w').close()"])

    def test_passes_without_changes(self):
        autogen.run_and_verify_no_changes([sys.executable, "-c", "pass"])

    def test_precheck_fails_on_dirty_tree(self):
        open("local-edit.txt", "w").close()
        with self.assertRaises(SystemExit):
            autogen.ensure_clean_tree()


if __name__ == "__main__":
    unittest.main()
