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

import os
from pathlib import Path
import subprocess
import time
from test.e2e.clients.python.framework.context import TestContext

import pytest

REPO_ROOT = Path(__file__).resolve().parents[4]
TEST_MANIFESTS_DIR = REPO_ROOT / "test" / "e2e" / "clients" / "python" / "test_manifests"
TEMPLATE_YAML_PATH = TEST_MANIFESTS_DIR / "sandbox_template.yaml"
SDK_PATH = REPO_ROOT / "clients" / "python" / "agentic-sandbox-client"
TEST_SCRIPT_PATH = REPO_ROOT / "test" / "e2e" / "clients" / "python" / "incluster_test_script.py"


@pytest.fixture(scope="module")
def tc():
    """Provides the required kubernetes api for E2E tests"""
    context = TestContext()
    yield context


@pytest.fixture(scope="function")
def temp_namespace(tc):
    """Creates and yields a temporary namespace for testing"""
    namespace = tc.create_temp_namespace(prefix="py-sdk-incluster-")
    yield namespace
    tc.delete_namespace(namespace)


def get_image_tag(env_var="IMAGE_TAG", default="latest"):
    return os.environ.get(env_var, default)


def get_image_prefix(env_var="IMAGE_PREFIX", default="kind.local/"):
    return os.environ.get(env_var, default)


@pytest.fixture(scope="function")
def sandbox_template(tc, temp_namespace):
    """Deploys the sandbox template into the test namespace"""
    image_tag = get_image_tag()
    image_prefix = get_image_prefix()
    with open(TEMPLATE_YAML_PATH, "r") as f:
        manifest = f.read().format(image_prefix=image_prefix, image_tag=image_tag)
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    return "python-sdk-test-template"


def test_incluster_compatibility_both_versions(tc, temp_namespace, sandbox_template):
    """Verifies that the Python SDK in-cluster mode works correctly under both older (v35.0.0) and newer (v36.0.0) kubernetes client versions."""
    
    # 1. Create RBAC and Pod manifests
    manifest = f"""
apiVersion: v1
kind: ServiceAccount
metadata:
  name: incluster-test-sa
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: incluster-test-role
rules:
- apiGroups: ["extensions.agents.x-k8s.io"]
  resources: ["sandboxclaims"]
  verbs: ["create", "get", "list", "watch", "delete"]
- apiGroups: ["agents.x-k8s.io"]
  resources: ["sandboxes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: incluster-test-role-binding
subjects:
- kind: ServiceAccount
  name: incluster-test-sa
roleRef:
  kind: Role
  name: incluster-test-role
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: Pod
metadata:
  name: py-sdk-incluster-test
spec:
  serviceAccountName: incluster-test-sa
  containers:
  - name: test-runner
    image: python:3.11-slim
    command: ["sleep", "3600"]
"""
    print("Applying service account, role binding and test pod...")
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    
    from kubernetes.client.exceptions import ApiException

    # Wait for Pod to be running
    print("Waiting for test pod to start...")
    core_v1 = tc.get_core_v1_api()
    timeout = time.monotonic() + 120
    while time.monotonic() < timeout:
        try:
            pod = core_v1.read_namespaced_pod(name="py-sdk-incluster-test", namespace=temp_namespace)
            if pod.status.phase == "Running":
                print("Test pod is running!")
                break
        except ApiException as e:
            if e.status != 404:
                raise
        time.sleep(2)
    else:
        pytest.fail("Test pod failed to start within timeout.")

    # 2. Copy the SDK codebase and script into the Pod
    print("Copying python SDK files to the pod...")
    env = os.environ.copy()
    if tc.kubeconfig_path:
        env["KUBECONFIG"] = tc.kubeconfig_path

    # Copy files
    subprocess.run(["kubectl", "exec", "-n", temp_namespace, "py-sdk-incluster-test", "--", "mkdir", "-p", "/app"], check=True, env=env)
    
    # Copy SDK directory recursively by tar-ing it (standard work-around for copying whole dirs with kubectl) using safe pipelining
    tar_proc = subprocess.Popen(
        ["tar", "-cf", "-", "-C", str(SDK_PATH), "."],
        stdout=subprocess.PIPE,
        env=env,
    )
    kubectl_cmd = [
        "kubectl", "exec", "-i", "-n", temp_namespace, "py-sdk-incluster-test",
        "--", "tar", "-xf", "-", "-C", "/app"
    ]
    k_proc = subprocess.Popen(
        kubectl_cmd,
        stdin=tar_proc.stdout,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
    )
    tar_proc.stdout.close()
    stdout, stderr = k_proc.communicate()
    tar_rc = tar_proc.wait()
    if tar_rc != 0 or k_proc.returncode != 0:
        raise RuntimeError(
            f"Tar copy failed. tar exit code: {tar_rc}, "
            f"kubectl exit code: {k_proc.returncode}, stderr: {stderr.decode()}"
        )

    # Copy the script itself
    subprocess.run(["kubectl", "cp", str(TEST_SCRIPT_PATH), f"{temp_namespace}/py-sdk-incluster-test:/app/incluster_test_script.py"], check=True, env=env)

    # Helper function to run a command inside the Pod
    def exec_in_pod(cmd_list):
        res = subprocess.run(["kubectl", "exec", "-n", temp_namespace, "py-sdk-incluster-test", "--"] + cmd_list, capture_output=True, text=True, env=env)
        print(f"CMD: {' '.join(cmd_list)}")
        print(f"Stdout:\n{res.stdout}")
        if res.stderr:
            print(f"Stderr:\n{res.stderr}")
        assert res.returncode == 0, f"Command failed with exit code {res.returncode}"

    # 3. Install the SDK package in editable mode
    print("Installing python SDK in editable mode inside the pod...")
    exec_in_pod(["env", "SETUPTOOLS_SCM_PRETEND_VERSION=0.1.0", "pip", "install", "-e", "/app"])

    # 4. PATH A: Test compatibility with kubernetes==35.0.0
    print("\n--- [PATH A] Testing compatibility with kubernetes==35.0.0 ---")
    exec_in_pod(["pip", "install", "kubernetes==35.0.0"])
    exec_in_pod(["python", "/app/incluster_test_script.py", sandbox_template, temp_namespace])
    print("[PATH A] Passed!")

    # 5. PATH B: Test compatibility with kubernetes==36.0.0 (or current latest)
    print("\n--- [PATH B] Testing compatibility with kubernetes==36.0.0 ---")
    exec_in_pod(["pip", "install", "kubernetes==36.0.0"])
    exec_in_pod(["python", "/app/incluster_test_script.py", sandbox_template, temp_namespace])
    print("[PATH B] Passed!")
