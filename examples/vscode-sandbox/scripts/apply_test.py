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

"""Test the VSCode example's deployment script without a cluster."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("apply.sh")


class ApplyTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.directory = Path(directory.name)
        self.log = self.directory / "kubectl.jsonl"
        kubectl = self.directory / "kubectl"
        kubectl.write_text(f"#!{sys.executable}\n" + """
import json
import os
import sys

args = sys.argv[1:]
command = args[2:]
if command[:3] == ["set", "image", "--local"]:
    print("rendered deployment")
if command == ["apply", "-f", "-"] and sys.stdin.read() != "rendered deployment\\n":
    sys.exit("Expected the rendered deployment on stdin")
with open(os.environ["KUBECTL_LOG"], "a") as log:
    print(json.dumps(args), file=log)
""")
        kubectl.chmod(0o755)
        self.env = os.environ.copy()
        self.env.pop("SANDBOX_ROUTER_IMAGE", None)
        self.env.update(
            PATH=str(self.directory) + os.pathsep + self.env["PATH"],
            KUBECONFIG=str(self.directory / "other-cluster"),
            KUBECTL_LOG=str(self.log),
        )

    def run_script(self, *args):
        return subprocess.run(
            ["bash", str(SCRIPT), *args], cwd=self.directory, env=self.env,
            capture_output=True, text=True, check=False,
        )

    def test_missing_arguments_fail_before_kubectl_runs(self):
        cases = (
            ((), "No sandbox-router image specified."),
            (("-i",), "requires an image value"),
            (("--sandbox-router-image",), "requires an image value"),
            (("-i", "router:test", "--kubeconfig"), "requires a file path"),
            (("--kubeconfig", "--sandbox-router-image", "router:test"), "requires a file path"),
        )
        for args, message in cases:
            with self.subTest(args=args):
                result = self.run_script(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertIn("Usage:", result.stdout)
                self.assertFalse(self.log.exists())

    def test_kubectl_uses_the_script_kubeconfig_or_the_flag(self):
        cases = (
            ((), str(SCRIPT.with_name("kubeconfig"))),
            (("--kubeconfig", "selected cluster"), "selected cluster"),
        )
        for args, expected in cases:
            with self.subTest(args=args):
                self.log.unlink(missing_ok=True)
                result = self.run_script("--sandbox-router-image", "router:test", *args)
                self.assertEqual(result.returncode, 0, result.stderr)
                calls = [json.loads(line) for line in self.log.read_text().splitlines()]
                self.assertTrue(all(call[:2] == ["--kubeconfig", expected] for call in calls))
                commands = [call[2:] for call in calls]
                self.assertTrue(any(
                    command[:3] == ["set", "image", "--local"]
                    and "sandbox-router=router:test" in command for command in commands
                ))
                self.assertIn(["apply", "-f", "-"], commands)
                self.assertTrue(any(
                    command[:2] == ["apply", "-k"]
                    and Path(command[2]).resolve() == SCRIPT.parents[1] / "overlays/kata-mshv"
                    for command in commands
                ))


if __name__ == "__main__":
    unittest.main()
