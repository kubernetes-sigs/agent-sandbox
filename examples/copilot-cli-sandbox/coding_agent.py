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

"""
Coding agent that uses Azure OpenAI (or any OpenAI-compatible API) to generate
Python code and executes it inside an Agent Sandbox pod.

The agent loop:
1. Takes a task from the user
2. Calls the LLM to generate Python code
3. Writes the code into a Sandbox and executes it
4. If execution fails, feeds the error back to the LLM for auto-fix
5. Repeats up to MAX_FIX_ATTEMPTS times
"""

import os
import sys

from openai import AzureOpenAI, OpenAI
from k8s_agent_sandbox import SandboxClient

MAX_FIX_ATTEMPTS = 3

SYSTEM_PROMPT = """You are an expert Python programmer. When given a task, \
generate clean, self-contained Python code that solves it.

Rules:
- Output ONLY executable Python code — no markdown fences, no explanations.
- The code must be completely self-contained with no external dependencies \
beyond the Python standard library.
- Include proper error handling.
- Print informative output so the user can see the results."""

FIX_PROMPT_TEMPLATE = """The following Python code failed with an error.
Fix the code so it runs successfully.

Output ONLY the corrected Python code — no markdown fences, no explanations.

Original task: {task}

Failed code:
{code}

Error:
{error}"""


def create_client():
    """Create an OpenAI client, auto-detecting Azure vs standard endpoints."""
    azure_endpoint = os.environ.get("AZURE_OPENAI_ENDPOINT")
    if azure_endpoint:
        return AzureOpenAI(
            azure_endpoint=azure_endpoint,
            api_key=os.environ.get("AZURE_OPENAI_API_KEY"),
            api_version=os.environ.get(
                "AZURE_OPENAI_API_VERSION", "2024-12-01-preview"),
        )
    api_key = os.environ.get("OPENAI_API_KEY")
    if api_key:
        base_url = os.environ.get("OPENAI_BASE_URL")
        return OpenAI(api_key=api_key, base_url=base_url)

    print("Error: Set AZURE_OPENAI_ENDPOINT + AZURE_OPENAI_API_KEY "
          "or OPENAI_API_KEY.")
    sys.exit(1)


def generate_code(client, model: str, task: str) -> str:
    """Ask the LLM to generate Python code for a task."""
    response = client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": task},
        ],
        temperature=0.2,
    )
    code = response.choices[0].message.content.strip()
    # Strip markdown fences if the model includes them anyway
    if code.startswith("```python"):
        code = code[len("```python"):].strip()
    if code.startswith("```"):
        code = code[3:].strip()
    if code.endswith("```"):
        code = code[:-3].strip()
    return code


def fix_code(client, model: str, task: str, code: str, error: str) -> str:
    """Ask the LLM to fix code that failed."""
    prompt = FIX_PROMPT_TEMPLATE.format(task=task, code=code, error=error)
    response = client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": prompt},
        ],
        temperature=0.2,
    )
    fixed = response.choices[0].message.content.strip()
    if fixed.startswith("```python"):
        fixed = fixed[len("```python"):].strip()
    if fixed.startswith("```"):
        fixed = fixed[3:].strip()
    if fixed.endswith("```"):
        fixed = fixed[:-3].strip()
    return fixed


def execute_in_sandbox(sandbox: SandboxClient, code: str):
    """Write code to the sandbox and execute it. Returns (stdout, success)."""
    sandbox.write("task.py", code)
    result = sandbox.run("python3 task.py", timeout=60)
    success = result.exit_code == 0
    output = result.stdout if success else (result.stderr or result.stdout)
    return output, success


def run_agent_loop(client, model: str, sandbox: SandboxClient):
    """Interactive loop: user gives tasks, agent generates and executes code."""
    print("=" * 60)
    print("Agent Sandbox — Coding Agent (Azure OpenAI)")
    print("=" * 60)
    print("Give me a coding task and I'll generate Python code,")
    print("execute it in a sandboxed environment, and show results.")
    print("Type 'exit' or 'quit' to stop.")
    print("=" * 60)

    while True:
        try:
            task = input("\nYou: ").strip()
        except (KeyboardInterrupt, EOFError):
            print("\nGoodbye!")
            break

        if not task:
            continue
        if task.lower() in ("exit", "quit"):
            print("Goodbye!")
            break

        # Generate
        print("\n[1/2] Generating code...")
        code = generate_code(client, model, task)
        print("-" * 40)
        print(code)
        print("-" * 40)

        # Execute + auto-fix loop
        for attempt in range(1, MAX_FIX_ATTEMPTS + 1):
            step = "2/2" if attempt == 1 else f"fix {attempt-1}/{MAX_FIX_ATTEMPTS}"
            print(f"\n[{step}] Executing in sandbox...")
            output, success = execute_in_sandbox(sandbox, code)

            if success:
                print("\n✅ Execution succeeded:")
                print(output)
                break

            print(f"\n❌ Execution failed (attempt {attempt}):")
            print(output)

            if attempt < MAX_FIX_ATTEMPTS:
                print(f"\n[fix] Asking LLM to fix the code...")
                code = fix_code(client, model, task, code, output)
                print("-" * 40)
                print(code)
                print("-" * 40)
        else:
            print(f"\n⛔ Failed after {MAX_FIX_ATTEMPTS} attempts.")


def main():
    model = os.environ.get("AZURE_OPENAI_DEPLOYMENT",
                           os.environ.get("OPENAI_MODEL", "gpt-4o"))
    template = os.environ.get("SANDBOX_TEMPLATE", "python-sandbox-template")
    namespace = os.environ.get("SANDBOX_NAMESPACE", "default")

    client = create_client()

    print(f"Connecting to sandbox (template={template}, ns={namespace})...")
    with SandboxClient(
        template_name=template,
        namespace=namespace,
    ) as sandbox:
        print("Sandbox ready!")
        run_agent_loop(client, model, sandbox)


if __name__ == "__main__":
    main()
