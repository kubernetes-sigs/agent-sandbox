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

"""End-to-end example: OpenHands on a pre-warmed Agent Sandbox pod.

Phase 1 (always): claim a warm pod and drive workspace operations directly —
server info, a shell command, a file round-trip. Needs only cluster access.

Phase 2 (optional): run a real agent conversation on the same workspace.
Enabled when LLM_API_KEY is set and the `openhands-tools` package (agent
presets) is installed.

Environment:
  SANDBOX_WARMPOOL     warm pool name        (default: agent-server-pool)
  SANDBOX_NAMESPACE    pool namespace        (default: default)
  SANDBOX_SESSION_KEY  pool session key      (default: unset — no auth header)
  LLM_API_KEY          enables phase 2
  LLM_MODEL            model for phase 2     (default: gpt-5.5)
  LLM_BASE_URL         optional LLM endpoint override

Run:  python example.py
"""

import os
import tempfile

from openhands_k8s_agent_sandbox import AgentSandboxWorkspace


def workspace_ops(workspace: AgentSandboxWorkspace) -> None:
    """Phase 1: raw workspace operations — no LLM involved."""
    info = workspace.get_server_info()
    print(f"agent-server info: {info}")

    result = workspace.execute_command("echo hello from a warm pod && uname -a")
    print(f"$ {result.command!r} -> exit {result.exit_code}\n{result.stdout}")

    with tempfile.TemporaryDirectory() as tmp_dir:
        local_path = os.path.join(tmp_dir, "roundtrip.txt")
        back_path = os.path.join(tmp_dir, "roundtrip.txt.back")
        with open(local_path, "w") as f:
            f.write("round-trip payload\n")
        upload = workspace.file_upload(local_path, "/workspace/roundtrip.txt")
        print(f"upload ok={upload.success}")
        download = workspace.file_download("/workspace/roundtrip.txt", back_path)
        print(f"download ok={download.success}")
        with open(back_path) as f:
            assert f.read() == "round-trip payload\n"
    print("file round-trip verified")


def agent_conversation(workspace: AgentSandboxWorkspace) -> None:
    """Phase 2: the standard SDK conversation, workspace class swapped."""
    from pydantic import SecretStr

    from openhands.sdk import LLM, Conversation
    from openhands.tools.preset.default import get_default_agent

    llm = LLM(
        usage_id="agent",
        model=os.getenv("LLM_MODEL", "gpt-5.5"),
        base_url=os.getenv("LLM_BASE_URL"),
        api_key=SecretStr(os.environ["LLM_API_KEY"]),
    )
    agent = get_default_agent(llm=llm, cli_mode=True)
    conversation = Conversation(agent=agent, workspace=workspace)
    try:
        conversation.send_message(
            "Write three facts about this environment into FACTS.txt, "
            "then print the file."
        )
        conversation.run()
        print(f"agent finished: {conversation.state.execution_status}")
    finally:
        conversation.close()


def main() -> None:
    with AgentSandboxWorkspace(
        warmpool=os.getenv("SANDBOX_WARMPOOL", "agent-server-pool"),
        namespace=os.getenv("SANDBOX_NAMESPACE", "default"),
        api_key=os.getenv("SANDBOX_SESSION_KEY"),
        ttl_s=1800,  # backstop: the controller reaps the claim if we die here
    ) as workspace:
        print(f"claimed warm workspace at {workspace.host}")
        workspace_ops(workspace)

        if not os.getenv("LLM_API_KEY"):
            print("LLM_API_KEY not set — skipping the agent-conversation phase")
            return
        try:
            import openhands.tools  # noqa: F401
        except ImportError:
            print("openhands-tools not installed — skipping the agent phase "
                  "(pip install openhands-tools)")
            return
        agent_conversation(workspace)


if __name__ == "__main__":
    main()
