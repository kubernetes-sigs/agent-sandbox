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

"""Unit tests for fix-go-generate — GOTOOLCHAIN pinning from go.mod."""

import importlib.util
import os
import tempfile
import unittest
from importlib.machinery import SourceFileLoader

# Load the extensionless fix-go-generate script via importlib (same pattern as
# dev/tools/verify_chart_version_test.py).
_SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
_loader = SourceFileLoader("fix_go_generate", os.path.join(_SCRIPT_DIR, "fix-go-generate"))
_spec = importlib.util.spec_from_loader("fix_go_generate", _loader)
fix_go_generate = importlib.util.module_from_spec(_spec)
_loader.exec_module(fix_go_generate)

GO_MOD = "module example.com/m\n\ngo 1.26.0\n\ntoolchain go1.27.1\n"


class GoToolchainEnvTest(unittest.TestCase):
    def toolchain_env(self, environ, go_mod=GO_MOD):
        with tempfile.TemporaryDirectory() as repo_root:
            with open(os.path.join(repo_root, "go.mod"), "w") as f:
                f.write(go_mod)
            return fix_go_generate.go_toolchain_env(repo_root, environ)

    def test_pins_toolchain_when_unset(self):
        env = self.toolchain_env({})
        self.assertEqual(env["GOTOOLCHAIN"], "go1.27.1+auto")

    def test_pins_go_line_without_toolchain_line(self):
        env = self.toolchain_env({}, "module example.com/m\n\ngo 1.27.1\n")
        self.assertEqual(env["GOTOOLCHAIN"], "go1.27.1+auto")

    def test_respects_explicit_gotoolchain(self):
        env = self.toolchain_env({"GOTOOLCHAIN": "local"})
        self.assertEqual(env["GOTOOLCHAIN"], "local")


if __name__ == "__main__":
    unittest.main()
