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
from test.e2e.clients.python.framework.context import TestContext

import pytest
from agentic_sandbox import SandboxClient

TEST_MANIFESTS_DIR = "test/e2e/clients/python/test_manifests"
TEMPLATE_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_template.yaml")
WARMPOOL_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_warmpool.yaml")

ROUTER_YAML_PATH = (
    "clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"
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
    tc.delete_namespace(namespace)


def get_image_tag(env_var="IMAGE_TAG", default="latest"):
    """Retrieves the image tag from environment variable or returns default"""
    return os.environ.get(env_var, default)


def get_image_prefix(env_var="IMAGE_PREFIX", default="kind.local/"):
    """Retrieves the image prefix from environment variable or returns default"""
    return os.environ.get(env_var, default)


@pytest.fixture(scope="function")
def deploy_router(tc, temp_namespace):
    """Deploys the sandbox router into the test namespace"""
    image_tag = get_image_tag()
    image_prefix = get_image_prefix()
    router_image = "{}sandbox-router:{}".format(image_prefix, image_tag)
    print(f"Using router image: {router_image}")

    with open(ROUTER_YAML_PATH, "r") as f:
        manifest = f.read().replace("IMAGE_PLACEHOLDER", router_image)

    print(f"Applying router manifest to namespace: {temp_namespace}")
    tc.apply_manifest_text(manifest, namespace=temp_namespace)

    print("Waiting for router deployment to be ready...")
    tc.wait_for_deployment_ready("sandbox-router-deployment", namespace=temp_namespace)


@pytest.fixture(scope="function")
def sandbox_template(tc, temp_namespace):
    """Deploys the sandbox template into the test namespace"""
    image_tag = get_image_tag()
    image_prefix = get_image_prefix()
    with open(TEMPLATE_YAML_PATH, "r") as f:
        manifest = f.read().format(image_prefix=image_prefix, image_tag=image_tag)
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    return "python-sdk-test-template"


@pytest.fixture(scope="function")
def sandbox_warmpool(tc, temp_namespace):
    """Deploys the sandbox warmpool into the test namespace"""
    with open(WARMPOOL_YAML_PATH, "r") as f:
        manifest = f.read()
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    print("Warmpool manifest applied.")

    tc.wait_for_warmpool_ready("python-sdk-warmpool", namespace=temp_namespace)
    print("Warmpool is ready.")


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
