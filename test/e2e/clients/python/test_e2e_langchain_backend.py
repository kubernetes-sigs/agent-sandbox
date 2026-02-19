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

import os
from typing import Optional

import pytest
from k8s_agent_sandbox import SandboxClient
from langchain_agent_sandbox import AgentSandboxBackend


REQUIRED_TEMPLATE_ENV = "LANGCHAIN_SANDBOX_TEMPLATE"


def _get_env(name: str, default: Optional[str] = None) -> Optional[str]:
    value = os.environ.get(name, default)
    return value if value not in (None, "") else None


def _should_skip() -> Optional[str]:
    template_name = _get_env(REQUIRED_TEMPLATE_ENV)
    if not template_name:
        return f"Missing {REQUIRED_TEMPLATE_ENV} env var"

    api_url = _get_env("LANGCHAIN_API_URL")
    gateway_name = _get_env("LANGCHAIN_GATEWAY_NAME")
    allow_tunnel = _get_env("LANGCHAIN_USE_TUNNEL", "0") == "1"

    if not api_url and not gateway_name and not allow_tunnel:
        return (
            "Set LANGCHAIN_API_URL or LANGCHAIN_GATEWAY_NAME to connect, "
            "or LANGCHAIN_USE_TUNNEL=1 to allow kubectl port-forward"
        )
    return None


def _require(condition: bool, message: str) -> None:
    if not condition:
        pytest.fail(message)


@pytest.mark.e2e
def test_langchain_backend_basic():
    skip_reason = _should_skip()
    if skip_reason:
        pytest.skip(skip_reason)

    template_name = _get_env(REQUIRED_TEMPLATE_ENV)
    namespace = _get_env("LANGCHAIN_NAMESPACE", "default")
    gateway_name = _get_env("LANGCHAIN_GATEWAY_NAME")
    gateway_namespace = _get_env("LANGCHAIN_GATEWAY_NAMESPACE", namespace)
    api_url = _get_env("LANGCHAIN_API_URL")
    server_port = int(_get_env("LANGCHAIN_SERVER_PORT", "8888"))
    root_dir = _get_env("LANGCHAIN_ROOT_DIR", "/app")

    with SandboxClient(
        template_name=template_name,
        namespace=namespace,
        gateway_name=gateway_name,
        gateway_namespace=gateway_namespace,
        api_url=api_url,
        server_port=server_port,
    ) as client:
        backend = AgentSandboxBackend(client, root_dir=root_dir)

        exec_result = backend.execute("echo 'hello from deepagents'")
        _require(
            "hello from deepagents" in exec_result.output,
            "Unexpected execute output",
        )
        _require(exec_result.exit_code == 0, "Unexpected execute exit code")

        file_path = "/langchain_e2e.txt"
        write_res = backend.write(file_path, "alpha\nbeta\nalpha\n")
        _require(write_res.error is None, f"Write failed: {write_res.error}")

        read_res = backend.read(file_path)
        _require("1: alpha" in read_res, "Missing first line in read output")
        _require("2: beta" in read_res, "Missing second line in read output")

        edit_res = backend.edit(file_path, "beta", "gamma", replace_all=False)
        _require(edit_res.error is None, f"Edit failed: {edit_res.error}")
        _require(edit_res.occurrences == 1, "Unexpected edit occurrence count")

        grep_res = backend.grep_raw("alpha", path="/")
        _require(isinstance(grep_res, list), "Unexpected grep result type")
        _require(
            any(match["path"].endswith("langchain_e2e.txt") for match in grep_res),
            "Grep did not return expected file",
        )

        glob_res = backend.glob_info("**/langchain_e2e.txt", path="/")
        _require(isinstance(glob_res, list), "Unexpected glob result type")
        _require(
            any(info["path"].endswith("langchain_e2e.txt") for info in glob_res),
            "Glob did not return expected file",
        )

        upload_responses = backend.upload_files([
            ("/nested/dir/extra.txt", b"payload"),
        ])
        _require(upload_responses[0].error is None, "Upload failed")

        downloads = backend.download_files(["/nested/dir/extra.txt"])
        _require(downloads[0].error is None, "Download failed")
        _require(downloads[0].content == b"payload", "Download payload mismatch")
