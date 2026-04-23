---
title: "Custom Environment"
linkTitle: "Custom Environment"
weight: 15
description: >
  Create a Sandbox with custom environment variables.
---
## Executing Commands with Custom Environment Variables

In many agentic workflows, you need to execute isolated commands inside the sandbox with specific environment variables (such as ephemeral API keys, testing flags, or dynamic paths) without permanently altering the global state of the container.

By extending the sandbox's FastAPI runtime, you can accept a dynamic `env` dictionary per request and merge it seamlessly into the specific execution context of that process.

## Prerequisites

- A running Kubernetes cluster with the [Agent Sandbox Controller]({{< ref "/docs/overview" >}}) installed.
- The [Sandbox Router](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/clients/python/agentic-sandbox-client/sandbox-router/README.md) deployed in your cluster.
- A `SandboxTemplate` named `python-sandbox-template` applied to your cluster, configured to use your custom FastAPI server as its entrypoint. See the [Python Runtime Sandbox]({{< ref "/docs/runtime-templates/python" >}}) guide for setup instructions.
- The [Python SDK]({{< ref "/docs/python-client" >}}) installed: `pip install k8s-agent-sandbox`.

### 1. The Custom Sandbox Runtime (Server-Side)

This code runs **inside** the sandbox pod. The `ExecuteRequest` model is extended to accept an optional dictionary of environment variables. When a command is triggered, it safely clones the system's current environment variables, merges the incoming ones, and injects them into the `subprocess.run` call.

```python
import os
import shlex
import string
import subprocess
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

class ExecuteRequest(BaseModel):
    command: dict[str, dict[str, str]]

@app.get("/", summary="Health Check")
async def health_check():
    """A simple health check endpoint to confirm the server is running."""
    return {"status": "ok", "message": "Sandbox Runtime is active."}

@app.post("/execute")
def execute_command(req: ExecuteRequest):
    try:
        current_env = os.environ.copy()

        if "env" in req.command:
            current_env.update(req.command["env"])

        raw_command = req.command["command"]["content"]
        expanded_string = string.Template(raw_command).safe_substitute(current_env)
        safe_command = shlex.split(expanded_string)

        result = subprocess.run(
            safe_command,
            capture_output=True,
            text=True,
            timeout=120,
            env=current_env,
        )

        # Return the exact schema the SDK expects
        return {
            "stdout": result.stdout,
            "stderr": result.stderr,
            "exitCode": result.returncode
        }
    except Exception as e:
        return {
            "stdout": "",
            "stderr": str(e),
            "exitCode": 1
        }
```

> Note: you can find the rest of the Sandbox Docker image [here](https://github.com/volatilemolotov/agent-sandbox/tree/main/site/content/docs/sandbox/custom_sandbox/custom_environment/source)

### 2. Client Execution Workflow

Unlike standard command executions, this client script sends a raw JSON payload directly through the SDK's run command to pass both the `command` and the `env` dictionary to the FastAPI server.

The following example demonstrates creating a sandbox, sending a command that requires a custom environment variable (`TEST=True`), and printing the modified output.

```python
from k8s_agent_sandbox import SandboxClient

# 1. Initialize the client
client = SandboxClient()

# 2. Create the sandbox using your custom runtime template
sandbox = client.create_sandbox("simple-sandbox-template")

# 3. Run a command and inject environment variables via the payload
# The FastAPI server parses this into the ExecuteRequest model
payload = {
    "command": {"content": "echo $TEST"},
    "env": {"TEST": "True"}
}
response = sandbox.commands.run(payload)

# 4. Verify the output
# Because of our custom runtime logic, this will print:
# True
print(response)
```
