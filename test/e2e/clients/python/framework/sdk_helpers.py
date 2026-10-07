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

import io
import os
import tempfile

TEST_MANIFESTS_DIR = "test/e2e/clients/python/test_manifests"
TEMPLATE_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_template.yaml")
WARMPOOL_YAML_PATH = os.path.join(TEST_MANIFESTS_DIR, "sandbox_warmpool.yaml")

ROUTER_YAML_PATH = (
    "clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"
)
GATEWAY_YAML_PATH = (
    "clients/python/agentic-sandbox-client/gateway-kind/gateway-kind.yaml"
)
GATEWAY_NAME = "kind-gateway"


def get_image_tag(env_var="IMAGE_TAG", default="latest"):
    """Retrieves the image tag from environment variable or returns default"""
    return os.environ.get(env_var, default)


def get_image_prefix(env_var="IMAGE_PREFIX", default="kind.local/"):
    """Retrieves the image prefix from environment variable or returns default"""
    return os.environ.get(env_var, default)


def wait_until_sandbox_routable(sandbox):
    """Probe the data path with GET before the first POST /execute.

    Claim Ready does not mean the router can dial the runtime yet. GET is
    still retried on 5xx; POST /execute is not.
    """
    sandbox.files.exists(".")



def run_sdk_tests(sandbox):
    """Runs basic SDK operations to validate functionality"""
    wait_until_sandbox_routable(sandbox)
    # Test execution
    result = sandbox.commands.run("echo 'Hello from SDK'")
    print(f"Run result: {result}")
    assert result.stdout == "Hello from SDK\n", f"Unexpected stdout: {result.stdout}"
    assert result.stderr == "", f"Unexpected stderr: {result.stderr}"
    assert result.exit_code == 0, f"Unexpected exit code: {result.exit_code}"

    # Test File Write / Read
    file_content = "This is a test file."
    file_path = "test.txt"  # Relative path inside the sandbox
    print(f"Writing content to '{file_path}'...")
    sandbox.files.write(file_path, file_content)

    print(f"Reading content from '{file_path}'...")
    read_content = sandbox.files.read(file_path).decode("utf-8")
    print(f"Read content: '{read_content}'")
    assert read_content == file_content, f"File content mismatch: {read_content}"

    with tempfile.TemporaryFile() as destination:
        written = sandbox.files.read_to(file_path, destination)
        destination.seek(0)
        streamed_content = destination.read().decode("utf-8")
    assert written == len(file_content.encode("utf-8"))
    assert streamed_content == file_content

    # Exercise the streaming multipart path through the real SDK, Router, and
    # runtime. Start after a prefix to verify that write() honors the caller's
    # current file position instead of rewinding the stream.
    stream_content = b"streamed through the Python SDK"
    stream = io.BytesIO(b"skip-" + stream_content)
    stream.seek(len(b"skip-"))
    stream_path = "streamed.txt"
    print(f"Streaming content to '{stream_path}'...")
    sandbox.files.write(stream_path, stream)
    assert not stream.closed, "write() must not close caller-owned streams"

    streamed_content = sandbox.files.read(stream_path)
    assert streamed_content == stream_content, (
        f"Streamed file content mismatch: {streamed_content!r}"
    )

