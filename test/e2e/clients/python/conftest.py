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
import tempfile

import pytest
import yaml
from k8s_agent_sandbox import SandboxClient
from k8s_agent_sandbox.models import (
    SandboxDirectConnectionConfig,
    SandboxGatewayConnectionConfig,
    SandboxLocalTunnelConnectionConfig,
    SandboxTracerConfig,
)

from test.e2e.clients.python.framework.context import TestContext
from test.e2e.clients.python.framework.sdk_helpers import (
    GATEWAY_NAME,
    GATEWAY_YAML_PATH,
    ROUTER_YAML_PATH,
    TEMPLATE_YAML_PATH,
    WARMPOOL_YAML_PATH,
    get_image_prefix,
    get_image_tag,
)


def pytest_configure(config):
    config.addinivalue_line(
        "markers", "example: testing examples migrated from legacy test_client"
    )


def pytest_addoption(parser):
    parser.addoption(
        "--warmpool-name",
        action="store",
        default="python-sdk-warmpool",
        help="The name of the sandbox warm pool to use for the test.",
    )
    parser.addoption(
        "--gateway-name",
        action="store",
        default=None,
        help="The name of the Gateway resource. If omitted, defaults to local port-forward mode.",
    )
    parser.addoption(
        "--api-url",
        action="store",
        default=None,
        help="Direct URL to router (e.g. http://localhost:8080)",
    )
    parser.addoption(
        "--namespace",
        action="store",
        default="testing-default",
        help="Namespace to create sandbox in",
    )
    parser.addoption(
        "--server-port",
        action="store",
        type=int,
        default=8888,
        help="Port the sandbox container listens on",
    )
    parser.addoption(
        "--enable-tracing",
        action="store_true",
        default=False,
        help="Enable OpenTelemetry tracing in the agentic-sandbox-client.",
    )
    parser.addoption(
        "--trace-service-name",
        action="store",
        default="sandbox-client-test",
        help="Trace service name",
    )


@pytest.fixture(scope="module")
def tc():
    """Provides the required kubernetes api for E2E tests"""
    context = TestContext()
    yield context


@pytest.fixture
def temp_namespace(
    request,
    tc,
):
    """Creates and yields a temporary namespace for testing"""
    namespace = request.config.getoption("--namespace")
    namespace = tc.create_temp_namespace(prefix=namespace)
    yield namespace
    tc.delete_namespace(namespace)


@pytest.fixture
def deploy_router(tc, temp_namespace):
    """Deploys the sandbox router into the test namespace"""
    image_tag = get_image_tag()
    image_prefix = get_image_prefix()
    router_image = f"{image_prefix}sandbox-router:{image_tag}"
    print(f"Using router image: {router_image}")

    with open(ROUTER_YAML_PATH, "r") as f:
        docs = list(yaml.safe_load_all(f))

    for doc in docs:
        if (
            doc
            and doc.get("kind") == "Deployment"
            and doc.get("metadata", {}).get("name") == "sandbox-router-deployment"
        ):
            containers = (
                doc.get("spec", {})
                .get("template", {})
                .get("spec", {})
                .get("containers", [])
            )
            for c in containers:
                if c.get("name") == "router":
                    c["image"] = router_image
                    env_vars = c.get("env", [])
                    for env_var in env_vars:
                        if env_var.get("name") == "ALLOW_UNAUTHENTICATED_ROUTER":
                            env_var["value"] = "true"

    manifest = yaml.safe_dump_all(docs)

    print(f"Applying router manifest to namespace: {temp_namespace}")
    tc.apply_manifest_text(manifest, namespace=temp_namespace)

    print("Waiting for router deployment to be ready...")
    tc.wait_for_deployment_ready("sandbox-router-deployment", namespace=temp_namespace)


@pytest.fixture
def deploy_gateway(tc, temp_namespace):
    """Deploys the sandbox gateway into the test namespace"""
    with open(GATEWAY_YAML_PATH, "r") as f:
        manifest = f.read()

    print(f"Applying gateway manifest to namespace: {temp_namespace}")
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    print("Waiting for gateway to get an address...")
    tc.wait_for_gateway_address(GATEWAY_NAME, namespace=temp_namespace)


@pytest.fixture
def sandbox_template(tc, temp_namespace):
    """Deploys the sandbox template into the test namespace"""
    image_tag = get_image_tag()
    image_prefix = get_image_prefix()
    with open(TEMPLATE_YAML_PATH, "r") as f:
        manifest = f.read().format(image_prefix=image_prefix, image_tag=image_tag)
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    return "python-sdk-test-template"


@pytest.fixture
def sandbox_warmpool(request, tc, sandbox_template, temp_namespace):
    """Deploys the sandbox warmpool into the test namespace"""
    warmpool_name = request.config.getoption("--warmpool-name")
    with open(WARMPOOL_YAML_PATH, "r") as f:
        manifest = yaml.safe_load(f)
    manifest["metadata"]["name"] = warmpool_name
    tc.apply_manifest_text(yaml.safe_dump(manifest), namespace=temp_namespace)
    print("Warmpool manifest applied.")

    tc.wait_for_warmpool_ready(warmpool_name, namespace=temp_namespace)
    print("Warmpool is ready.")
    return warmpool_name


@pytest.fixture
def sandbox_coldpool(tc, temp_namespace, sandbox_template):
    """Deploys a zero-replica sandbox warmpool for cold start tests"""
    manifest = f"""apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: python-sdk-coldpool
spec:
  replicas: 0
  sandboxTemplateRef:
    name: {sandbox_template}
"""
    tc.apply_manifest_text(manifest, namespace=temp_namespace)
    print("Coldpool manifest applied.")
    return "python-sdk-coldpool"
