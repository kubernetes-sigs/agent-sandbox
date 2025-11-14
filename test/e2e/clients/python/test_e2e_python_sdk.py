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

import pytest
import os
import yaml
import asyncio
import time
from kubernetes import client, config
from kubernetes.client import ApiException
from agentic_sandbox import SandboxClient

# Get namespace from environment variable, default to 'default'
NAMESPACE = os.getenv("TEST_NAMESPACE", "default")
IMAGE_TAG = os.getenv("IMAGE_TAG", "latest")

TEMPLATE_NAME = "e2e-pytest-template"

TEMPLATE_MANIFEST = f"""
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: {TEMPLATE_NAME}
  namespace: {NAMESPACE}
spec:
  podTemplate:
    metadata:
      labels:
        app: python-sandbox-pytest
    spec:
      containers:
      - name: python-sandbox
        image: kind.local/python-runtime-sandbox:{IMAGE_TAG}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 8888
"""

WARMPOOL_NAME = "e2e-pytest-warmpool"

WARMPOOL_MANIFEST = f"""
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: {WARMPOOL_NAME}
  namespace: {NAMESPACE}
spec:
  replicas: 1
  sandboxTemplateRef:
    name: {TEMPLATE_NAME}
"""

@pytest.fixture(scope="module")
def k8s_api():
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    api = client.CustomObjectsApi()
    return api

@pytest.fixture(scope="module", autouse=True)
def create_template(k8s_api):
    template_body = yaml.safe_load(TEMPLATE_MANIFEST)
    try:
        k8s_api.create_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sandboxtemplates",
            body=template_body,
        )
        print(f"SandboxTemplate '{TEMPLATE_NAME}' created in namespace '{NAMESPACE}'.")
    except client.ApiException as e:
        if e.status == 409:
            print(f"SandboxTemplate '{TEMPLATE_NAME}' already exists.")
        else:
            raise

    yield

    try:
        k8s_api.delete_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sandboxtemplates",
            name=TEMPLATE_NAME,
        )
        print(f"SandboxTemplate '{TEMPLATE_NAME}' deleted.")
    except client.ApiException as e:
        if e.status == 404:
            print(f"SandboxTemplate '{TEMPLATE_NAME}' not found for deletion.")
        else:
            print(f"Error deleting SandboxTemplate '{TEMPLATE_NAME}': {e}")

@pytest.fixture(scope="function")
def create_warmpool(k8s_api, create_template): # Depends on create_template
    warmpool_body = yaml.safe_load(WARMPOOL_MANIFEST)
    try:
        k8s_api.create_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sandboxwarmpools",
            body=warmpool_body,
        )
        print(f"SandboxWarmPool '{WARMPOOL_NAME}' created.")
    except ApiException as e:
        if e.status == 409:
            print(f"SandboxWarmPool '{WARMPOOL_NAME}' already exists.")
        else:
            raise

    # Wait for warmpool to be ready
    for i in range(30): # Wait up to 60 seconds
        try:
            warmpool = k8s_api.get_namespaced_custom_object(
                group="extensions.agents.x-k8s.io",
                version="v1alpha1",
                namespace=NAMESPACE,
                plural="sandboxwarmpools",
                name=WARMPOOL_NAME,
            )
            if warmpool.get("status", {}).get("readyReplicas", 0) >= 1:
                print(f"SandboxWarmPool '{WARMPOOL_NAME}' is ready with {warmpool['status']['readyReplicas']} replicas.")
                break
        except ApiException as e:
            print(f"Error getting warmpool status: {e}")
        if i == 29:
            pytest.fail(f"SandboxWarmPool '{WARMPOOL_NAME}' did not become ready in time.")
        time.sleep(2)

    yield

    try:
        k8s_api.delete_namespaced_custom_object(
            group="extensions.agents.x-k8s.io",
            version="v1alpha1",
            namespace=NAMESPACE,
            plural="sandboxwarmpools",
            name=WARMPOOL_NAME,
        )
        print(f"SandboxWarmPool '{WARMPOOL_NAME}' deleted.")
    except ApiException as e:
        if e.status == 404:
            print(f"SandboxWarmPool '{WARMPOOL_NAME}' not found for deletion.")
        else:
            print(f"Error deleting SandboxWarmPool '{WARMPOOL_NAME}': {e}")

@pytest.mark.asyncio
async def test_python_sdk_sandbox_execution():
    """
    Tests the Python SDK SandboxClient.
    """
    print(f"--- Running test_python_sdk_sandbox_execution in namespace {NAMESPACE} ---")
    try:
        with SandboxClient(template_name=TEMPLATE_NAME, namespace=NAMESPACE) as sandbox:
            await asyncio.sleep(60)  # Wait for the sandbox to be fully ready

            print("\n--- Testing Command Execution ---")
            command_to_run = "echo 'Hello from pytest sandbox!'"
            print(f"Executing command: '{command_to_run}'")

            result = sandbox.run(command_to_run)

            print(f"Stdout: {result.stdout.strip()}")
            print(f"Stderr: {result.stderr.strip()}")
            print(f"Exit Code: {result.exit_code}")

            assert result.exit_code == 0
            assert result.stdout.strip() == "Hello from pytest sandbox!"
            print("--- Command Execution Test Passed! ---")

            print("\n--- Testing File Write/Read ---")
            file_path = "test_sdk.txt"
            file_content = "SDK test content."
            sandbox.write(file_path, file_content)
            read_content = sandbox.read(file_path).decode('utf-8')
            assert read_content == file_content
            print("--- File Write/Read Test Passed! ---")

    except Exception as e:
        print(f"\n--- An error occurred during the test: {e} ---")
        pytest.fail(f"Test failed due to exception: {e}")
    finally:
        print("--- Test finished ---")

# @pytest.mark.asyncio
# async def test_python_sdk_warmpool_execution(create_warmpool): # Use the warmpool fixture
#     """
#     Tests the Python SDK SandboxClient with a WarmPool.
#     """
#     print(f"--- Running test_python_sdk_warmpool_execution in namespace {NAMESPACE} ---")
#     try:
#         with SandboxClient(template_name=TEMPLATE_NAME, namespace=NAMESPACE) as sandbox:
#             print("\n--- Testing Command Execution (WarmPool) ---")
#             command_to_run = "echo 'Hello from warmpooled sandbox!'"
#             print(f"Executing command: '{command_to_run}'")

#             result = sandbox.run(command_to_run)

#             print(f"Stdout: {result.stdout.strip()}")
#             assert result.exit_code == 0
#             assert result.stdout.strip() == "Hello from warmpooled sandbox!"
#             print("--- Command Execution Test Passed (WarmPool)! ---")

#     except Exception as e:
#         print(f"\n--- An error occurred during the test: {e} ---")
#         pytest.fail(f"Test failed due to exception: {e}")
#     finally:
#         print("--- WarmPool Test finished ---")

# pip install pytest pytest-asyncio kubernetes requests PyYAML
