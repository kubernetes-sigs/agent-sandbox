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
import time
from test.e2e.clients.python.framework.context import TestContext

import pytest
import yaml
from agentic_sandbox import SandboxClient

# Assuming the template manifest from the other test is reusable
TEMPLATE_MANIFEST = """
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: python-sdk-test-template
spec:
  podTemplate:
    metadata:
      labels:
        app: python-sandbox
        sandbox: sdk-test-sandbox
    spec:
      containers:
      - name: python-sandbox
        image: kind.local/python-runtime-sandbox:{image_tag}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
"""

WARMPOOL_MANIFEST = """
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: python-sdk-warmpool
spec:
  replicas: 1
  sandboxTemplateRef:
    name: python-sdk-test-template
"""

ROUTER_YAML_PATH = (
    "clients/python/agentic-sandbox-client/sandbox_router/sandbox_router.yaml"
)


@pytest.fixture(scope="module")
def tc():
    """Provides the required kubernetes api for E2E tests"""
    context = TestContext()
    yield context


@pytest.fixture(scope="function")
def temp_namespace(tc):
    """Creates and yields a temporary namespace for testing"""
    namespace = tc.create_temp_namespace(prefix="py-sdk-e2e-")
    yield namespace
    tc.delete_namespace(namespace)  # Uncomment for cleanup


def get_image_tag(env_var="IMAGE_TAG", default="latest"):
    """Retrieves the image tag from environment variable or returns default"""
    return os.environ.get(env_var, default)


@pytest.fixture(scope="function")
def deploy_router(tc, temp_namespace):
    """Deploys the sandbox router into the test namespace"""
    image_tag = get_image_tag()
    router_image = "kind.local/sandbox-router:{}".format(image_tag)
    print(f"Using router image: {router_image}")

    with open(ROUTER_YAML_PATH, "r") as f:
        router_manifests = list(yaml.safe_load_all(f.read()))

    modified_manifests = []
    for manifest in router_manifests:
        if not manifest:
            continue
        manifest["metadata"]["namespace"] = temp_namespace
        if manifest["kind"] == "Deployment":
            manifest["spec"]["template"]["spec"]["containers"][0][
                "image"
            ] = router_image
        modified_manifests.append(manifest)

    router_manifest_text = yaml.dump_all(modified_manifests)

    print(f"Applying router manifest to namespace: {temp_namespace}")
    tc.apply_manifest_text(router_manifest_text, namespace=temp_namespace)

    print("Waiting for router deployment to be ready...")
    tc.wait_for_deployment_ready(
        "sandbox-router-deployment", namespace=temp_namespace, timeout=180
    )


@pytest.fixture(scope="function")
def sandbox_template(tc, temp_namespace):
    """Deploys the sandbox template into the test namespace"""
    image_tag = get_image_tag()
    manifest = TEMPLATE_MANIFEST.format(image_tag=image_tag)
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    return "python-sdk-test-template"


@pytest.fixture(scope="function")
def sandbox_warmpool(tc, temp_namespace):
    """Deploys the sandbox warmpool into the test namespace"""
    tc.apply_manifest_text(WARMPOOL_MANIFEST, namespace=temp_namespace)
    print("Warmpool manifest applied.")
    time.sleep(10)  # Wait 10 seconds for the warmpool to start provisioning


def run_sdk_tests(sandbox):
    """Runs basic SDK operations to validate functionality"""
    # Test execution
    result = sandbox.run("echo 'Hello from SDK'")
    print(f"Run result: {result}")
    assert result.stdout == "Hello from SDK\n", f"Unexpected stdout: {result.stdout}"
    assert result.stderr == "", f"Unexpected stderr: {result.stderr}"
    assert result.exit_code == 0, f"Unexpected exit code: {result.exit_code}"

    # Test File Write / Read
    file_content = "This is a test file."
    file_path = "test.txt"  # Relative path inside the sandbox
    print(f"Writing content to '{file_path}'...")
    sandbox.write(file_path, file_content)

    print(f"Reading content from '{file_path}'...")
    read_content = sandbox.read(file_path).decode("utf-8")
    print(f"Read content: '{read_content}'")
    assert read_content == file_content, f"File content mismatch: {read_content}"


def test_python_sdk_router_mode(tc, temp_namespace, sandbox_template, deploy_router):
    """Tests the Python SDK in Sandbox Router (Developer/Tunnel) mode without warmpool."""
    try:
        with SandboxClient(
            template_name=sandbox_template,
            namespace=temp_namespace,
        ) as sandbox:
            print("\n--- Running SDK tests without warmpool ---")
            run_sdk_tests(sandbox)
            print("SDK test without warmpool passed!")

    except Exception as e:
        pytest.fail(f"SDK test without warmpool failed: {e}")


def test_python_sdk_router_mode_warmpool(
    tc, temp_namespace, sandbox_template, deploy_router, sandbox_warmpool
):
    """Tests the Python SDK in Sandbox Router mode with warmpool."""
    try:
        with SandboxClient(
            template_name=sandbox_template,
            namespace=temp_namespace,
        ) as sandbox:
            print("\n--- Running SDK tests with warmpool ---")
            run_sdk_tests(sandbox)
            print("SDK test with warmpool passed!")

    except Exception as e:
        pytest.fail(f"SDK test with warmpool failed: {e}")


# Todo: Enable Gateway in kind cluster
# def test_python_sdk_gateway_mode(tc, temp_namespace, sandbox_template, deploy_router):
#     """Tests the Python SDK in Gateway (Production/Discovery) mode."""
#     gateway_name = "sandbox-router-gateway"  # Assuming this is the gateway name
#     try:
#         with SandboxClient(
#             template_name=sandbox_template,
#             namespace=temp_namespace,
#             gateway_name=gateway_name,
#             gateway_namespace=temp_namespace,
#         ) as sandbox:
#             print("\n--- Running SDK tests in Gateway mode ---")
#             run_sdk_tests(sandbox)
#             print("SDK test in Gateway mode passed!")

#     except Exception as e:
#         pytest.fail(f"SDK test in Gateway mode failed: {e}")

# def test_python_sdk_gateway_mode_warmpool(tc, temp_namespace, sandbox_template, deploy_router, sandbox_warmpool):
#     """Tests the Python SDK in Gateway (Production/Discovery) mode."""
#     gateway_name = "sandbox-router-gateway"  # Assuming this is the gateway name
#     try:
#         with SandboxClient(
#             template_name=sandbox_template,
#             namespace=temp_namespace,
#             gateway_name=gateway_name,
#             gateway_namespace=temp_namespace,
#         ) as sandbox:
#             print("\n--- Running SDK tests in Gateway mode ---")
#             run_sdk_tests(sandbox)
#             print("SDK test in Gateway mode passed!")

#     except Exception as e:
#         pytest.fail(f"SDK test in Gateway mode failed: {e}")
