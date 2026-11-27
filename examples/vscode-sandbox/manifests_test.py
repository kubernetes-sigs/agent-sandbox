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

"""Render the example's Kustomizations without a Kubernetes cluster."""

from copy import deepcopy
from pathlib import Path, PurePosixPath
import shlex
import subprocess
import unittest

import yaml


EXAMPLE = Path(__file__).resolve().parent


def render(variant):
    cmd = ["go", "run", "sigs.k8s.io/kustomize/kustomize/v5@v5.8.1", "build", variant]
    output = subprocess.check_output(cmd, cwd=EXAMPLE, text=True)
    return {(obj["kind"], obj["metadata"]["name"]): obj for obj in yaml.safe_load_all(output)}


def sandbox_pod_spec(objects):
    return objects[("Sandbox", "sandbox-example")]["spec"]["podTemplate"]["spec"]


class ManifestsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.base = render("base")

    def test_code_server_uses_a_mounted_password_authenticated_entrypoint(self):
        pod = sandbox_pod_spec(self.base)
        container, = pod["containers"]
        env = {item["name"]: item["value"] for item in container["env"]}
        script_path = PurePosixPath(shlex.split(env["ENVBUILDER_INIT_SCRIPT"])[-1])
        self.assertTrue(script_path.is_absolute(), "envbuilder reuses the existing workspace checkout")
        mount = next(item for item in container["volumeMounts"] if item["mountPath"] == str(script_path.parent))
        self.assertTrue(mount["readOnly"])
        volume = next(item for item in pod["volumes"] if item["name"] == mount["name"])
        script = self.base[("ConfigMap", volume["configMap"]["name"])]["data"][script_path.name]
        code_server = next(line for line in script.splitlines() if line.startswith("exec /usr/bin/code-server "))
        self.assertIn("--auth=password", shlex.split(code_server))
        self.assertIn(mount["mountPath"], env["ENVBUILDER_IGNORE_PATHS"].split(","))

    def test_isolation_overlays_preserve_base_pod_settings(self):
        base = sandbox_pod_spec(self.base)
        for name, runtime in (
            ("gvisor", "gvisor"),
            ("kata", "kata-qemu"),
            ("kata-mshv", "kata-vm-isolation"),
        ):
            with self.subTest(overlay=name):
                expected = deepcopy(base)
                expected["runtimeClassName"] = runtime
                if name == "kata-mshv":
                    resources = expected["containers"][0]["resources"]
                    resources["requests"]["memory"] = "4Gi"
                    resources["limits"]["memory"] = "4Gi"
                rendered = sandbox_pod_spec(render(f"overlays/{name}"))
                self.assertEqual(rendered, expected)

    def test_base_reserves_and_limits_ephemeral_storage_to_8_gib(self):
        resources = sandbox_pod_spec(self.base)["containers"][0]["resources"]
        self.assertEqual(resources["requests"]["ephemeral-storage"], "8Gi")
        self.assertEqual(resources["limits"]["ephemeral-storage"], "8Gi")


if __name__ == "__main__":
    unittest.main()
