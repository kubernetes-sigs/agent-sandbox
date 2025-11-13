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


""" Below example shows basic usage of the aio-sandbox sdk, example taken and modified from https://github.com/agent-infra/sandbox/tree/main?tab=readme-ov-file#basic-usage """

import os
from agent_sandbox import Sandbox

# Initialize the Sandbox client using the GATEWAY_URL environment variable
base_url = os.environ.get("GATEWAY_URL", "http://localhost:8080")
client = Sandbox(base_url=base_url)

# Get the home directory of the sandbox context
home_dir = client.sandbox.get_context().home_dir

# Run a simple shell command to list files in the sandbox home directory
result = client.shell.exec_command(command="ls -la", timeout=10)
print(result.data.output)

# Read the contents of the .bashrc file in the sandbox home directory
content = client.file.read_file(file=f"{home_dir}/.bashrc")
print(content.data.content)

# Take a screenshot of the sandbox browser (headless automation)
screenshot_path = "sandbox_screenshot.png"
with open(screenshot_path, "wb") as f:
    for chunk in client.browser.screenshot():
        f.write(chunk)
print(f"Screenshot saved to {screenshot_path}")