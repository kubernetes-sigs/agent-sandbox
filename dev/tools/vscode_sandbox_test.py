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

"""Regression tests for the VSCode sandbox deployment script's command line."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "examples/vscode-sandbox/scripts/apply.sh"


class VSCodeSandboxTest(unittest.TestCase):
    def run_script(self, *args, env_overrides=None):
        env = os.environ.copy()
        env.pop("SANDBOX_ROUTER_IMAGE", None)
        env.update(env_overrides or {})
        with tempfile.TemporaryDirectory() as directory:
            return subprocess.run(
                ["bash", str(SCRIPT), *args], cwd=directory, env=env,
                capture_output=True, text=True, check=False,
            )

    def test_missing_image_reports_usage(self):
        result = self.run_script()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("No sandbox-router image specified.", result.stdout + result.stderr)
        self.assertIn("Usage:", result.stdout + result.stderr)
        self.assertNotIn("unbound variable", result.stdout + result.stderr)

    def test_image_option_requires_a_value(self):
        for option in ("-i", "--sandbox-router-image"):
            with self.subTest(option=option):
                result = self.run_script(option)
                output = result.stdout + result.stderr
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("requires an image value", output)
                self.assertIn("Usage:", output)
                self.assertNotIn("unbound variable", output)

    def test_deploys_go_router_with_selected_image_from_any_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            kubectl = directory / "kubectl"
            kubectl.write_text(f"#!{sys.executable}\n" + '''
import json
import os
from pathlib import Path
import sys
import yaml

args = sys.argv[1:]
stdin = sys.stdin.read() if "-" in args else ""
resources = []
if args[0] == "apply":
    for i, arg in enumerate(args):
        if arg != "-f":
            continue
        filename = args[i + 1]
        if filename.startswith("https://"):
            continue
        content = stdin if filename == "-" else Path(filename).read_text()
        resources.extend(yaml.safe_load_all(content))
    if "-k" in args:
        directory = Path(args[args.index("-k") + 1])
        if not (directory / "kustomization.yaml").is_file():
            sys.exit("Kustomization not found")
with open(os.environ["KUBECTL_LOG"], "a") as log:
    log.write(json.dumps({"resources": resources,
                          "kubeconfig": os.environ.get("KUBECONFIG")}) + "\\n")
if args[:2] == ["set", "image"] and "--local" in args:
    resource = yaml.safe_load(Path(args[args.index("-f") + 1]).read_text())
    image_arg = next(arg for arg in args if "=" in arg and not arg.startswith("--"))
    name, image = image_arg.split("=", 1)
    container = next(c for c in resource["spec"]["template"]["spec"]["containers"]
                     if c["name"] == name)
    container["image"] = image
    print(json.dumps(resource))
''')
            kubectl.chmod(0o755)
            log = directory / "kubectl.jsonl"
            kubeconfig = str(directory / "selected-kubeconfig")
            result = self.run_script(
                "--sandbox-router-image", "example.com/sandbox-router-go:test",
                env_overrides={
                    "PATH": str(directory) + os.pathsep + os.environ["PATH"],
                    "KUBECTL_LOG": str(log),
                    "KUBECONFIG": kubeconfig,
                },
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            calls = [json.loads(line) for line in log.read_text().splitlines()]

        resources = {(resource["kind"], resource["metadata"]["name"]): resource
                     for call in calls for resource in call["resources"]}
        deployment = resources[("Deployment", "sandbox-router")]
        self.assertEqual(deployment["metadata"]["namespace"], "agent-sandbox-system")
        pod_spec = deployment["spec"]["template"]["spec"]
        self.assertEqual(pod_spec["serviceAccountName"], "sandbox-router")
        self.assertEqual(pod_spec["containers"][0]["image"],
                         "example.com/sandbox-router-go:test")
        self.assertTrue(all(call["kubeconfig"] == kubeconfig for call in calls))
        account = resources[("ServiceAccount", "sandbox-router")]
        self.assertEqual(account["metadata"]["namespace"], "agent-sandbox-system")
        role = resources[("ClusterRole", "sandbox-router")]
        self.assertIn({"apiGroups": [""], "resources": ["pods"],
                       "verbs": ["get", "list", "watch"]}, role["rules"])
        binding = resources[("ClusterRoleBinding", "sandbox-router")]
        self.assertEqual(binding["roleRef"]["name"], "sandbox-router")
        self.assertIn({"kind": "ServiceAccount", "name": "sandbox-router",
                       "namespace": "agent-sandbox-system"}, binding["subjects"])
        service = resources[("Service", "sandbox-router-svc")]
        self.assertEqual(service["metadata"]["namespace"], "agent-sandbox-system")
        pod_labels = deployment["spec"]["template"]["metadata"]["labels"]
        for label, value in service["spec"]["selector"].items():
            self.assertEqual(pod_labels[label], value)


if __name__ == "__main__":
    unittest.main()
